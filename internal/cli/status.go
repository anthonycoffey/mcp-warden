package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/anthonycoffey/mcp-warden/internal/classify"
	"github.com/anthonycoffey/mcp-warden/internal/history"
	"github.com/anthonycoffey/mcp-warden/internal/procscan"
	"github.com/anthonycoffey/mcp-warden/internal/state"
)

// instanceView is an instance plus liveness, as shown by status.
type instanceView struct {
	state.Instance
	Stale bool `json:"stale"`
}

func loadInstances(dir string) ([]instanceView, error) {
	entries, err := state.List(dir)
	if err != nil {
		return nil, err
	}
	out := make([]instanceView, 0, len(entries))
	for _, e := range entries {
		in := e.Instance
		out = append(out, instanceView{Instance: in, Stale: !procscan.Alive(in.WardenPID, in.WardenCreatedAt)})
	}
	return out, nil
}

// liveChildPIDs returns child PIDs of live instances, for classification.
func liveChildPIDs(views []instanceView) classify.Instances {
	inst := classify.Instances{ChildPIDs: map[int]bool{}}
	for _, v := range views {
		if !v.Stale && v.ChildPID > 0 {
			inst.ChildPIDs[v.ChildPID] = true
		}
	}
	return inst
}

type scanSummary struct {
	Time       time.Time `json:"time"`
	Orphaned   int       `json:"orphaned"`
	OrphanRSS  uint64    `json:"orphanedRss"`
	Unmanaged  int       `json:"unmanaged"`
	Detached   int       `json:"detached"`
	Managed    int       `json:"managed"`
	Unreadable int       `json:"unreadable"`
}

func summarizeScan(s history.Scan) scanSummary {
	r := s.Result
	return scanSummary{
		Time: s.Time, Orphaned: r.Count(classify.Orphaned), OrphanRSS: r.Memory(classify.Orphaned),
		Unmanaged: r.Count(classify.Unmanaged), Detached: r.Count(classify.Detached),
		Managed: r.Count(classify.Managed), Unreadable: r.Unreadable,
	}
}

func statusCmd(env Env, args []string) int {
	fs := newFlags(env, "status", "[--json]")
	asJSON := fs.Bool("json", false, "output JSON")
	dirFlag := stateDirFlag(fs)
	if code, ok := parse(fs, args); !ok {
		return code
	}
	dir := resolveDir(*dirFlag, state.DefaultDir)
	views, err := loadInstances(dir)
	if err != nil {
		return fail(env, "%v", err)
	}
	var summary *scanSummary
	if s, err := history.LoadScan(dir); err == nil {
		ss := summarizeScan(s)
		summary = &ss
	}

	if *asJSON {
		return writeJSON(env, struct {
			Instances []instanceView `json:"instances"`
			Scan      *scanSummary   `json:"scan,omitempty"`
		}{views, summary})
	}

	if len(views) == 0 {
		fmt.Fprintln(env.Stdout, "No mcp-warden instances. Wrap a server with `mcp-warden run --name <name> -- <command>`.")
	} else {
		tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSTATE\tPID\tCHILD\tUPTIME\tRESTARTS\tREQS\tERRS\tP50\tP95\tLAST EXIT")
		for _, v := range views {
			st := string(v.State)
			if v.Stale {
				st = "stale"
			}
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\n",
				v.Name, st, v.WardenPID, dash(v.ChildPID), humanAge(time.Since(v.StartedAt)),
				v.Restarts, v.Counts.Requests, v.Counts.Errors,
				ms(v.LatencyMs.P50), ms(v.LatencyMs.P95), truncate(orDash(v.LastExit), 40))
		}
		tw.Flush()
	}

	if summary != nil {
		fmt.Fprintf(env.Stdout, "\nLast scan %s ago: %s\n", humanAge(time.Since(summary.Time)), scanLine(*summary))
		if summary.Orphaned > 0 {
			fmt.Fprintln(env.Stdout, "Run `mcp-warden scan` for details, `mcp-warden scan --kill` to clean up.")
		}
	} else {
		fmt.Fprintln(env.Stdout, "\nNo machine scan yet. Run `mcp-warden scan` to look for unmanaged and orphaned MCP servers.")
	}
	if n := countStale(views); n > 0 {
		fmt.Fprintf(env.Stdout, "%d stale instance(s): their mcp-warden died. Run `mcp-warden reap`.\n", n)
	}
	return 0
}

func scanLine(s scanSummary) string {
	first := fmt.Sprintf("%d orphaned MCP tree%s", s.Orphaned, plural(s.Orphaned))
	if s.Orphaned > 0 {
		first += fmt.Sprintf(" (%s)", humanBytes(s.OrphanRSS))
	}
	parts := make([]string, 0, 4)
	parts = append(parts, first,
		fmt.Sprintf("%d unmanaged", s.Unmanaged),
		fmt.Sprintf("%d detached", s.Detached),
		fmt.Sprintf("%d managed", s.Managed))
	line := strings.Join(parts, ", ")
	if s.Unreadable > 0 {
		line += fmt.Sprintf("; %d unreadable process%s skipped", s.Unreadable, pluralES(s.Unreadable))
	}
	return line
}

func countStale(views []instanceView) int {
	n := 0
	for _, v := range views {
		if v.Stale {
			n++
		}
	}
	return n
}

func dash(pid int) string {
	if pid == 0 {
		return "-"
	}
	return fmt.Sprint(pid)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func ms(v int64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%dms", v)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func pluralES(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}

func writeJSON(env Env, v any) int {
	enc := json.NewEncoder(env.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fail(env, "%v", err)
	}
	return 0
}
