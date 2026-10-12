// Package classify decides, for every MCP-looking process tree on the
// machine, who owns it: mcp-warden, a live MCP client, nobody (an orphan), or
// something else. It is a pure function of a process snapshot, so all of the
// policy here is unit-tested without touching the OS.
package classify

import (
	"cmp"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/anthonycoffey/mcp-warden/internal/procscan"
)

// Class is the ownership verdict for a tree.
type Class string

const (
	// Managed trees run under a live mcp-warden.
	Managed Class = "managed"
	// Unmanaged trees belong to a live MCP client but are not wrapped.
	Unmanaged Class = "unmanaged"
	// Orphaned trees have lost their owner. Only these are ever killed.
	Orphaned Class = "orphaned"
	// Detached trees are owned by some other live process (e.g. a terminal).
	Detached Class = "detached"
)

// rank orders classes for display: the actionable ones first.
func (c Class) rank() int {
	return map[Class]int{Orphaned: 0, Unmanaged: 1, Detached: 2, Managed: 3}[c]
}

// ClientRule recognizes an MCP client process by executable name or command
// line. Either regexp may be nil.
type ClientRule struct {
	Label   string
	Name    *regexp.Regexp
	Cmdline *regexp.Regexp
}

func (r ClientRule) matches(p procscan.Process) bool {
	return (r.Name != nil && r.Name.MatchString(p.Name)) ||
		(r.Cmdline != nil && p.Cmdline != "" && r.Cmdline.MatchString(p.Cmdline))
}

// Config is the matching policy.
type Config struct {
	Match   []*regexp.Regexp // a process is an MCP candidate if its command line matches any
	Exclude []*regexp.Regexp // …and matches none of these
	Clients []ClientRule
	// Wrappers are launchers that sit between a client and a server (shells,
	// runtimes, npx). Ownership is decided by looking through them.
	Wrappers []string
	// Reapers adopt orphans on Unix (PID 1 always counts).
	Reapers []string
}

// DefaultConfig is the built-in policy. Patterns are deliberately broad;
// a false positive is only ever reported, never killed, unless it is also an
// orphan and the user asks.
func DefaultConfig() Config {
	re := regexp.MustCompile
	return Config{
		Match: []*regexp.Regexp{
			re(`(?i)@modelcontextprotocol[\\/]`),
			re(`(?i)(^|[^a-z0-9])mcp([^a-z0-9]|$)`), // "mcp" as a token: mcp-server-x, figma-mcp, mcp_server.py
			re(`(?i)mcp-?server`),
			re(`(?i)fastmcp`),
		},
		Clients: []ClientRule{
			{Label: "Claude", Name: re(`(?i)^claude(\.exe)?$`)},
			{Label: "Claude Code", Cmdline: re(`(?i)@anthropic-ai[\\/]claude-code`)},
			{Label: "Cursor", Name: re(`(?i)^cursor(\.exe)?$`)},
			{Label: "VS Code", Name: re(`(?i)^code(\.exe)?$|^code - insiders(\.exe)?$|^code-insiders$`)},
			{Label: "Windsurf", Name: re(`(?i)^windsurf(\.exe)?$`)},
			{Label: "Zed", Name: re(`(?i)^zed(\.exe)?$`)},
			{Label: "Codex", Name: re(`(?i)^codex(\.exe)?$`), Cmdline: re(`(?i)@openai[\\/]codex`)},
			{Label: "Gemini CLI", Cmdline: re(`(?i)@google[\\/]gemini-cli`)},
		},
		Wrappers: []string{
			"cmd", "conhost", "node", "npx", "npm", "pnpm", "yarn", "bun", "bunx", "deno",
			"uv", "uvx", "python", "python3", "pythonw", "pipx",
			"sh", "bash", "zsh", "dash", "fish", "powershell", "pwsh", "env", "docker",
		},
		Reapers: []string{"init", "systemd", "launchd"},
	}
}

// Instances describes processes owned by live mcp-warden instances (from
// their state files).
type Instances struct {
	ChildPIDs map[int]bool
}

// Tree is one finding: a root MCP process and everything below it.
type Tree struct {
	Key     string             `json:"key"`
	Class   Class              `json:"class"`
	Client  string             `json:"client,omitempty"` // for Unmanaged
	Reason  string             `json:"reason"`
	Root    procscan.Process   `json:"root"`
	Members []procscan.Process `json:"members"` // root first, then descendants
	RSS     uint64             `json:"rss"`
}

// Result is a classified snapshot.
type Result struct {
	Trees      []Tree `json:"trees"`
	Unreadable int    `json:"unreadable"`
}

// Count returns how many trees have class c.
func (r Result) Count(c Class) int {
	n := 0
	for _, t := range r.Trees {
		if t.Class == c {
			n++
		}
	}
	return n
}

// Memory returns the total RSS of trees with class c.
func (r Result) Memory(c Class) uint64 {
	var n uint64
	for _, t := range r.Trees {
		if t.Class == c {
			n += t.RSS
		}
	}
	return n
}

// TreeKey identifies a tree stably across scans: PIDs get reused, (PID,
// creation time) does not.
func TreeKey(p procscan.Process) string {
	return fmt.Sprintf("%d@%d", p.PID, p.CreateTime.UnixMilli())
}

// Classify groups MCP candidates into trees and assigns each a Class.
func Classify(procs []procscan.Process, inst Instances, cfg Config) Result {
	idx := procscan.NewIndex(procs)
	c := classifier{cfg: cfg, idx: idx, inst: inst}

	var res Result
	for _, p := range procs {
		if p.Unreadable {
			res.Unreadable++
		}
	}
	for _, p := range procs {
		if !c.candidate(p) || c.hasCandidateAncestor(p) {
			continue
		}
		members := append([]procscan.Process{p}, idx.Descendants(p.PID)...)
		t := Tree{Key: TreeKey(p), Root: p, Members: members}
		for _, m := range members {
			t.RSS += m.RSS
		}
		t.Class, t.Client, t.Reason = c.owner(p, members)
		res.Trees = append(res.Trees, t)
	}
	slices.SortFunc(res.Trees, func(a, b Tree) int {
		return cmp.Or(
			cmp.Compare(a.Class.rank(), b.Class.rank()),
			cmp.Compare(b.RSS, a.RSS),
			cmp.Compare(a.Root.PID, b.Root.PID),
		)
	})
	return res
}

type classifier struct {
	cfg  Config
	idx  *procscan.Index
	inst Instances
}

func baseName(name string) string {
	n := strings.ToLower(filepath.Base(name))
	return strings.TrimSuffix(n, ".exe")
}

// IsWarden reports whether p is an mcp-warden process.
func IsWarden(p procscan.Process) bool { return baseName(p.Name) == "mcp-warden" }

func (c classifier) client(p procscan.Process) (string, bool) {
	for _, r := range c.cfg.Clients {
		if r.matches(p) {
			return r.Label, true
		}
	}
	return "", false
}

func (c classifier) candidate(p procscan.Process) bool {
	if p.Unreadable || p.Cmdline == "" || IsWarden(p) {
		return false
	}
	if _, isClient := c.client(p); isClient {
		return false // e.g. a client started with --mcp-config
	}
	// "mcp-warden" itself must not make a process look like an MCP server
	// (e.g. a shell running `mcp-warden scan`), but other evidence in the
	// same command line still counts.
	evidence := wardenWord.ReplaceAllString(p.Cmdline, "")
	return anyMatch(c.cfg.Match, evidence) && !anyMatch(c.cfg.Exclude, p.Cmdline)
}

var wardenWord = regexp.MustCompile(`(?i)mcp-warden`)

func (c classifier) hasCandidateAncestor(p procscan.Process) bool {
	return slices.ContainsFunc(c.idx.Ancestors(p), c.candidate)
}

// owner walks up from the root, looking through wrapper processes, until it
// finds whoever is responsible for the tree.
func (c classifier) owner(root procscan.Process, members []procscan.Process) (Class, string, string) {
	for _, m := range members {
		if c.inst.ChildPIDs[m.PID] {
			return Managed, "", "listed in a live mcp-warden state file"
		}
	}
	for _, a := range c.idx.Ancestors(root) {
		if IsWarden(a) {
			return Managed, "", fmt.Sprintf("supervised by mcp-warden (pid %d)", a.PID)
		}
		if label, ok := c.client(a); ok {
			return Unmanaged, label, fmt.Sprintf("launched by %s (pid %d) without mcp-warden", label, a.PID)
		}
		if a.PID == 1 || slices.Contains(c.cfg.Reapers, baseName(a.Name)) {
			return Orphaned, "", fmt.Sprintf("re-parented to %s (pid %d)", a.Name, a.PID)
		}
		if !slices.Contains(c.cfg.Wrappers, baseName(a.Name)) {
			return Detached, "", fmt.Sprintf("owned by %s (pid %d)", a.Name, a.PID)
		}
	}
	// The chain ended at a parent that no longer exists.
	top := root
	if anc := c.idx.Ancestors(root); len(anc) > 0 {
		top = anc[len(anc)-1]
	}
	return Orphaned, "", fmt.Sprintf("parent pid %d is no longer running", top.PPID)
}

func anyMatch(res []*regexp.Regexp, s string) bool {
	return slices.ContainsFunc(res, func(r *regexp.Regexp) bool { return r.MatchString(s) })
}
