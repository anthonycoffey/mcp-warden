package classify

import (
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/anthonycoffey/mcp-warden/internal/procscan"
)

var t0 = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

// proc builds a process created sec seconds after t0.
func proc(pid, ppid int, name, cmd string, sec int) procscan.Process {
	return procscan.Process{PID: pid, PPID: ppid, Name: name, Cmdline: cmd,
		CreateTime: t0.Add(time.Duration(sec) * time.Second), RSS: 10 << 20}
}

// A Windows desktop with Claude running one server directly, one through
// mcp-warden, an orphaned npx→node pair, and a server started in a terminal.
func desktop() []procscan.Process {
	return []procscan.Process{
		proc(4, 0, "System", "", 0),
		proc(500, 4, "explorer.exe", `C:\Windows\explorer.exe`, 1),
		proc(1000, 500, "Claude.exe", `"C:\...\Claude.exe"`, 10),
		proc(1001, 1000, "Claude.exe", `"C:\...\Claude.exe" --type=utility`, 11),

		// Unmanaged: Claude → cmd → node (npx) → node (server)
		proc(2000, 1001, "cmd.exe", `cmd.exe /c npx -y @modelcontextprotocol/server-github`, 20),
		proc(2001, 2000, "node.exe", `node npx-cli.js -y @modelcontextprotocol/server-github`, 21),
		proc(2002, 2001, "node.exe", `node C:\npm-cache\server-github\dist\index.js`, 22),

		// Managed: Claude → mcp-warden → npx
		proc(3000, 1001, "mcp-warden.exe", `mcp-warden run --name figma -- npx -y figma-mcp`, 30),
		proc(3001, 3000, "node.exe", `node npx-cli.js -y figma-mcp`, 31),

		// Orphaned: parent 9999 is gone.
		proc(4000, 9999, "node.exe", `node npx-cli.js -y @heroku/mcp-server`, 40),
		proc(4001, 4000, "node.exe", `node C:\npm-cache\heroku\index.js`, 41),
		proc(4002, 4000, "conhost.exe", `conhost.exe 0x4`, 41),

		// Detached: started by hand in Windows Terminal.
		proc(5000, 500, "WindowsTerminal.exe", `WindowsTerminal.exe`, 50),
		proc(5001, 5000, "pwsh.exe", `pwsh.exe`, 51),
		proc(5002, 5001, "python.exe", `python Z:\repos\mcp-for-gtm\mcp_server.py`, 52),

		// Not MCP at all.
		proc(6000, 500, "node.exe", `node next dev`, 60),
		// Unreadable.
		{PID: 7000, PPID: 4, Name: "MsMpEng.exe", Unreadable: true},
	}
}

func byRoot(r Result) map[int]Tree {
	m := map[int]Tree{}
	for _, t := range r.Trees {
		m[t.Root.PID] = t
	}
	return m
}

func memberPIDs(t Tree) []int {
	var out []int
	for _, m := range t.Members {
		out = append(out, m.PID)
	}
	return out
}

func TestClassifyDesktop(t *testing.T) {
	res := Classify(desktop(), Instances{}, DefaultConfig())
	trees := byRoot(res)

	tests := []struct {
		root    int
		class   Class
		client  string
		members []int
	}{
		{2000, Unmanaged, "Claude", []int{2000, 2001, 2002}},
		{3001, Managed, "", []int{3001}},
		{4000, Orphaned, "", []int{4000, 4001, 4002}},
		{5002, Detached, "", []int{5002}},
	}
	for _, tt := range tests {
		got, ok := trees[tt.root]
		if !ok {
			t.Errorf("no tree rooted at %d; trees: %v", tt.root, trees)
			continue
		}
		if got.Class != tt.class || got.Client != tt.client {
			t.Errorf("tree %d: class=%s client=%q (%s), want %s %q",
				tt.root, got.Class, got.Client, got.Reason, tt.class, tt.client)
		}
		if !slices.Equal(memberPIDs(got), tt.members) {
			t.Errorf("tree %d: members %v, want %v", tt.root, memberPIDs(got), tt.members)
		}
	}
	if len(res.Trees) != len(tests) {
		t.Errorf("got %d trees, want %d (nested candidates must not form their own trees)", len(res.Trees), len(tests))
	}
	if res.Unreadable != 1 {
		t.Errorf("unreadable = %d", res.Unreadable)
	}
	if res.Trees[0].Class != Orphaned {
		t.Error("orphans should sort first")
	}
	if got := trees[4000].RSS; got != 30<<20 {
		t.Errorf("orphan tree RSS = %d, want sum of members", got)
	}
}

func TestPIDReuseMakesOrphan(t *testing.T) {
	// The server's PPID (100) now belongs to a process created after it.
	procs := []procscan.Process{
		proc(100, 1, "Claude.exe", `Claude.exe`, 50),
		proc(200, 100, "node.exe", `node mcp-server-x`, 10),
	}
	tr := byRoot(Classify(procs, Instances{}, DefaultConfig()))[200]
	if tr.Class != Orphaned {
		t.Errorf("class = %s (%s), want orphaned", tr.Class, tr.Reason)
	}
}

func TestReparentedToInitIsOrphan(t *testing.T) {
	procs := []procscan.Process{
		proc(1, 0, "systemd", "/sbin/init", 0),
		proc(800, 1, "node", "node /usr/lib/node_modules/@modelcontextprotocol/server-fs/dist/index.js", 10),
	}
	tr := byRoot(Classify(procs, Instances{}, DefaultConfig()))[800]
	if tr.Class != Orphaned {
		t.Errorf("class = %s (%s)", tr.Class, tr.Reason)
	}
}

func TestUserSubreaperIsOrphan(t *testing.T) {
	procs := []procscan.Process{
		proc(1, 0, "systemd", "/sbin/init", 0),
		proc(900, 1, "systemd", "/usr/lib/systemd/systemd --user", 5),
		proc(901, 900, "uv", "uv tool run mcp-server-fetch", 10),
	}
	tr := byRoot(Classify(procs, Instances{}, DefaultConfig()))[901]
	if tr.Class != Orphaned {
		t.Errorf("class = %s (%s)", tr.Class, tr.Reason)
	}
}

func TestOrphanBehindWrapper(t *testing.T) {
	// The server's direct parent is alive, but it is only a wrapper whose own
	// parent (the client) has died.
	procs := []procscan.Process{
		proc(300, 9999, "cmd.exe", `cmd.exe /c start-server.bat`, 10),
		proc(301, 300, "python.exe", `python -m mcp_server_time`, 11),
	}
	tr := byRoot(Classify(procs, Instances{}, DefaultConfig()))[301]
	if tr.Class != Orphaned {
		t.Errorf("class = %s (%s)", tr.Class, tr.Reason)
	}
}

func TestManagedViaStateFile(t *testing.T) {
	// Even if the warden is not visible as an ancestor (e.g. a racy
	// snapshot), a PID listed in a live state file is managed.
	procs := []procscan.Process{proc(400, 9999, "node.exe", "node mcp-server-x", 1)}
	tr := byRoot(Classify(procs, Instances{ChildPIDs: map[int]bool{400: true}}, DefaultConfig()))[400]
	if tr.Class != Managed {
		t.Errorf("class = %s", tr.Class)
	}
}

func TestClaudeCodeIsAClient(t *testing.T) {
	procs := []procscan.Process{
		proc(10, 1, "zsh", "-zsh", 0),
		proc(11, 10, "node", "node /usr/local/lib/node_modules/@anthropic-ai/claude-code/cli.js --mcp-config x.json", 1),
		proc(12, 11, "node", "node playwright-mcp", 2),
	}
	res := Classify(procs, Instances{}, DefaultConfig())
	trees := byRoot(res)
	if _, ok := trees[11]; ok {
		t.Error("a client process must never be a candidate, even with 'mcp' in its args")
	}
	if tr := trees[12]; tr.Class != Unmanaged || tr.Client != "Claude Code" {
		t.Errorf("tree 12 = %s %q", tr.Class, tr.Client)
	}
}

func TestExcludeAndMatchPatterns(t *testing.T) {
	procs := []procscan.Process{
		proc(1, 0, "init", "init", 0),
		proc(20, 1, "vim", "vim notes/mcp-ideas.md", 1),
		proc(21, 1, "my-tool", "my-tool --serve", 1),
	}
	cfg := DefaultConfig()
	cfg.Exclude = []*regexp.Regexp{regexp.MustCompile(`^vim `)}
	cfg.Match = append(cfg.Match, regexp.MustCompile(`my-tool --serve`))
	trees := byRoot(Classify(procs, Instances{}, cfg))
	if _, ok := trees[20]; ok {
		t.Error("excluded process was classified")
	}
	if _, ok := trees[21]; !ok {
		t.Error("custom match pattern ignored")
	}
}

func TestPatternBoundaries(t *testing.T) {
	cfg := DefaultConfig()
	match := func(s string) bool { return anyMatch(cfg.Match, s) }
	for _, yes := range []string{
		"npx -y @modelcontextprotocol/server-filesystem /tmp",
		"node figma-mcp/dist/index.js",
		"python mcp_server.py",
		"uvx mcp-server-fetch",
		"python -m fastmcp run server.py",
		"docker run -i --rm mcp/github",
	} {
		if !match(yes) {
			t.Errorf("should match: %s", yes)
		}
	}
	for _, no := range []string{"node next dev", "git commit -m 'mcpherson'", "vacmp"} {
		if match(no) {
			t.Errorf("should not match: %s", no)
		}
	}
}

func TestWardenNeverCandidate(t *testing.T) {
	procs := []procscan.Process{
		proc(50, 9999, "mcp-warden.exe", "mcp-warden scan", 1),
		// A shell running a warden command is not an MCP server…
		proc(51, 9999, "bash.exe", `bash -c "mcp-warden status"`, 1),
		// …but a real server that merely lives under a mcp-warden path is.
		proc(52, 9999, "node.exe", `node Z:\repos\mcp-warden\examples\figma-mcp.js`, 1),
	}
	trees := byRoot(Classify(procs, Instances{}, DefaultConfig()))
	if len(trees) != 1 {
		t.Fatalf("trees = %v", trees)
	}
	if _, ok := trees[52]; !ok {
		t.Error("server under a mcp-warden path was missed")
	}
}
