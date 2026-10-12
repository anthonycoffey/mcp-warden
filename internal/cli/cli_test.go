package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anthonycoffey/mcp-warden/internal/classify"
	"github.com/anthonycoffey/mcp-warden/internal/history"
	"github.com/anthonycoffey/mcp-warden/internal/procscan"
	"github.com/anthonycoffey/mcp-warden/internal/state"
)

func runCLI(args ...string) (string, int) {
	var out bytes.Buffer
	code := Main(args, Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out})
	return out.String(), code
}

func TestDispatch(t *testing.T) {
	if out, code := runCLI(); code != 2 || !strings.Contains(out, "Commands:") {
		t.Errorf("no args: %d %q", code, out)
	}
	if out, code := runCLI("bogus"); code != 2 || !strings.Contains(out, `unknown command "bogus"`) {
		t.Errorf("unknown: %d %q", code, out)
	}
	if out, code := runCLI("version"); code != 0 || !strings.HasPrefix(out, "mcp-warden ") {
		t.Errorf("version: %d %q", code, out)
	}
	if _, code := runCLI("status", "-h"); code != 0 {
		t.Errorf("-h should exit 0, got %d", code)
	}
	if _, code := runCLI("status", "--nope"); code != 2 {
		t.Errorf("bad flag should exit 2, got %d", code)
	}
}

func TestRunValidatesArgs(t *testing.T) {
	if _, code := runCLI("run", "--name", "x"); code != 2 {
		t.Errorf("missing command: %d", code)
	}
	if out, code := runCLI("run", "--name", "../evil", "--", "x"); code != 1 || !strings.Contains(out, "--name") {
		t.Errorf("bad name: %d %q", code, out)
	}
}

func TestStatusShowsStaleInstancesAndScan(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	// A live instance (this test process) and a dead one.
	self, _ := procscan.CreateTime(os.Getpid())
	state.WriteFile(state.InstancePath(dir, "live"), state.Instance{
		Schema: 1, Name: "live", WardenPID: os.Getpid(), WardenCreatedAt: self,
		State: state.Running, StartedAt: now, Restarts: 2,
	})
	state.WriteFile(state.InstancePath(dir, "dead"), state.Instance{
		Schema: 1, Name: "dead", WardenPID: 999999, State: state.Running, StartedAt: now,
	})
	history.SaveScan(dir, history.Scan{Time: now, Result: classify.Result{Trees: []classify.Tree{
		{Class: classify.Orphaned, RSS: 300 << 20}, {Class: classify.Unmanaged},
	}}})

	out, code := runCLI("status", "--state-dir", dir)
	if code != 0 {
		t.Fatalf("status = %d: %s", code, out)
	}
	for _, want := range []string{"live", "running", "dead", "stale", "1 orphaned MCP tree (300 MB)", "1 unmanaged", "mcp-warden reap"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}

	out, _ = runCLI("status", "--state-dir", dir, "--json")
	var js struct {
		Instances []instanceView `json:"instances"`
		Scan      *scanSummary   `json:"scan"`
	}
	if err := json.Unmarshal([]byte(out), &js); err != nil {
		t.Fatal(err)
	}
	if len(js.Instances) != 2 || js.Scan == nil || js.Scan.Orphaned != 1 {
		t.Errorf("json = %+v", js)
	}
}

func TestReapRemovesDeadInstanceWithNothingRunning(t *testing.T) {
	dir := t.TempDir()
	state.WriteFile(state.InstancePath(dir, "gone"), state.Instance{Schema: 1, Name: "gone", WardenPID: 999999, ChildPID: 999998})
	out, code := runCLI("reap", "--state-dir", dir, "--dry-run")
	if code != 0 || !strings.Contains(out, "left nothing running") {
		t.Errorf("dry run: %d %q", code, out)
	}
	if _, err := os.Stat(state.InstancePath(dir, "gone")); err != nil {
		t.Error("dry run must not delete state files")
	}
	runCLI("reap", "--state-dir", dir)
	if _, err := os.Stat(state.InstancePath(dir, "gone")); !os.IsNotExist(err) {
		t.Error("reap should delete the stale state file")
	}
}

func TestHistoryCommand(t *testing.T) {
	dir := t.TempDir()
	w, _ := history.OpenWriter(dir)
	root := procscan.Process{PID: 5, Cmdline: "npx -y leaky-mcp", CreateTime: time.Now()}
	tree := classify.Tree{Key: classify.TreeKey(root), Class: classify.Orphaned, Root: root, RSS: 50 << 20,
		Members: []procscan.Process{root}}
	w.Append([]history.Event{history.NewEvent(history.Appeared, tree, time.Now())})
	w.Close()

	out, code := runCLI("history", "--state-dir", dir, "--since", "1d")
	if code != 0 || !strings.Contains(out, "leaky-mcp") || !strings.Contains(out, "1 orphaned tree") {
		t.Errorf("history: %d\n%s", code, out)
	}
	if _, code := runCLI("history", "--state-dir", dir, "--since", "soon"); code != 1 {
		t.Error("bad --since should fail")
	}
}

func TestPolicyFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(ConfigPath(dir), []byte(`{"match":["my-tool"],"exclude":["^vim"],"clients":[{"label":"Acme","name":"^acme$"}]}`), 0o644)
	cfg, err := loadPolicy(dir, []string{"extra"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	def := classify.DefaultConfig()
	if len(cfg.Match) != len(def.Match)+2 || len(cfg.Exclude) != 1 || len(cfg.Clients) != len(def.Clients)+1 {
		t.Errorf("policy not merged: %d match, %d exclude, %d clients", len(cfg.Match), len(cfg.Exclude), len(cfg.Clients))
	}
	if _, err := loadPolicy(dir, []string{"("}, nil); err == nil {
		t.Error("invalid regexp should fail")
	}
}

func TestFormatting(t *testing.T) {
	cases := []struct{ got, want string }{
		{humanBytes(0), "-"},
		{humanBytes(512), "512 B"},
		{humanBytes(12 << 20), "12 MB"},
		{humanBytes(3 << 30), "3.0 GB"},
		{humanAge(45 * time.Second), "45s"},
		{humanAge(90 * time.Minute), "1h"},
		{humanAge(72 * time.Hour), "3d"},
		{truncate("abcdef", 4), "abc…"},
		{truncate("abc", 4), "abc"},
	}
	for i, c := range cases {
		if c.got != c.want {
			t.Errorf("case %d: got %q, want %q", i, c.got, c.want)
		}
	}
	for in, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "90m": 90 * time.Minute} {
		if got, err := parseSince(in); err != nil || got != want {
			t.Errorf("parseSince(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "xd", "-1h", "soon"} {
		if _, err := parseSince(bad); err == nil {
			t.Errorf("parseSince(%q) should fail", bad)
		}
	}
}

func TestLeftovers(t *testing.T) {
	t0 := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	inst := state.Instance{ChildPID: 100, ChildStartedAt: at(10)}

	// Server exited; its grandchild lives on in the server's process group.
	idx := procscan.NewIndex([]procscan.Process{
		{PID: 200, PPID: 1, PGID: 100, CreateTime: at(11)},
		{PID: 201, PPID: 200, PGID: 100, CreateTime: at(12)},
	})
	if got := leftovers(idx, inst); len(got) != 1 || got[0].PID != 200 {
		t.Errorf("dead server: leftovers = %+v, want root 200 (201 is its descendant)", got)
	}

	// Server alive: its tree, with group members it already covers not repeated.
	idx = procscan.NewIndex([]procscan.Process{
		{PID: 100, PPID: 1, PGID: 100, CreateTime: at(10)},
		{PID: 101, PPID: 100, PGID: 100, CreateTime: at(11)},
	})
	if got := leftovers(idx, inst); len(got) != 1 || got[0].PID != 100 {
		t.Errorf("live server: leftovers = %+v", got)
	}

	// PID 100 now belongs to an unrelated, newer process: touch nothing.
	idx = procscan.NewIndex([]procscan.Process{
		{PID: 100, PPID: 1, PGID: 100, CreateTime: at(500)},
		{PID: 300, PPID: 100, PGID: 100, CreateTime: at(501)},
	})
	if got := leftovers(idx, inst); got != nil {
		t.Errorf("reused pid: leftovers = %+v, want none", got)
	}
}
