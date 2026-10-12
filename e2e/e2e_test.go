// Package e2e drives the real mcp-warden binary against the fakeserver binary,
// covering the acceptance criteria that unit tests cannot: real pipes, real
// process trees, real signals.
package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthonycoffey/mcp-warden/internal/classify"
	"github.com/anthonycoffey/mcp-warden/internal/cli"
	"github.com/anthonycoffey/mcp-warden/internal/history"
	"github.com/anthonycoffey/mcp-warden/internal/procscan"
	"github.com/anthonycoffey/mcp-warden/internal/state"
)

var (
	wardenBin string
	fakeBin   string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mcp-warden-e2e")
	if err != nil {
		panic(err)
	}
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	wardenBin = filepath.Join(dir, "mcp-warden"+exe)
	// The "mcp" token in the name makes the fake server match the default
	// scan patterns, like a real server would.
	fakeBin = filepath.Join(dir, "fake-mcp-server"+exe)
	for bin, pkg := range map[string]string{wardenBin: "../cmd/mcp-warden", fakeBin: "../internal/fakeserver"} {
		out, err := exec.Command("go", "build", "-o", bin, pkg).CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", pkg, err, out)
			os.Exit(1)
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// warden is a running `mcp-warden run` with the test acting as the client.
type warden struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bufio.Scanner
	stderr *syncBuffer
	dir    string
}

func startWarden(t *testing.T, dir string, env []string, args ...string) *warden {
	t.Helper()
	full := append([]string{"run", "--name", "fake", "--state-dir", dir,
		"--backoff-base", "10ms", "--grace", "1s"}, args...)
	full = append(full, "--", fakeBin)
	cmd := exec.Command(wardenBin, full...)
	cmd.Env = append(os.Environ(), env...)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w := &warden{t: t, cmd: cmd, stdin: stdin, out: bufio.NewScanner(stdout), stderr: stderr, dir: dir}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return w
}

// syncBuffer is written by exec's copying goroutine and read by the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (w *warden) send(s string) {
	w.t.Helper()
	if _, err := io.WriteString(w.stdin, s+"\n"); err != nil {
		w.t.Fatal(err)
	}
}

// recv returns the next message with a deadline.
func (w *warden) recv() map[string]any {
	w.t.Helper()
	ch := make(chan string, 1)
	go func() {
		if w.out.Scan() {
			ch <- w.out.Text()
		} else {
			ch <- ""
		}
	}()
	select {
	case line := <-ch:
		if line == "" {
			w.t.Fatalf("stdout closed; stderr:\n%s", w.stderr)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			w.t.Fatalf("non-JSON on stdout (protocol channel polluted): %q", line)
		}
		return m
	case <-time.After(10 * time.Second):
		w.t.Fatalf("timed out; stderr:\n%s", w.stderr)
		return nil
	}
}

// response waits for the response to id, skipping notifications.
func (w *warden) response(id float64) map[string]any {
	w.t.Helper()
	for {
		m := w.recv()
		if got, ok := m["id"].(float64); ok && got == id {
			return m
		}
	}
}

func (w *warden) handshake() {
	w.t.Helper()
	w.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e"}}}`)
	if r := w.response(1); r["result"] == nil {
		w.t.Fatalf("initialize failed: %v", r)
	}
	w.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}

func (w *warden) instance() state.Instance {
	w.t.Helper()
	var inst state.Instance
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if state.ReadFile(state.InstancePath(w.dir, "fake"), &inst) == nil && inst.ChildPID != 0 {
			return inst
		}
		time.Sleep(50 * time.Millisecond)
	}
	w.t.Fatal("state file never written")
	return inst
}

func (w *warden) wait(timeout time.Duration) error {
	w.t.Helper()
	done := make(chan error, 1)
	go func() { done <- w.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		w.t.Fatal("mcp-warden did not exit")
		return nil
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid file %s never written", path)
	return 0
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// ---------------------------------------------------------------------------

func TestRelayRestartAndCleanShutdown(t *testing.T) {
	dir := t.TempDir()
	w := startWarden(t, dir, nil)
	w.handshake()

	w.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if r := w.response(2); r["result"] == nil {
		t.Fatalf("tools/list: %v", r)
	}

	w.send(`{"jsonrpc":"2.0","id":3,"method":"crash"}`)
	r := w.response(3)
	errObj, _ := r["error"].(map[string]any)
	data, _ := errObj["data"].(map[string]any)
	if data["reason"] != "server_restarted" {
		t.Fatalf("crash response: %v", r)
	}

	w.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo"}}`)
	if r := w.response(4); r["result"] == nil {
		t.Fatalf("call after restart: %v", r)
	}
	eventually(t, "state file shows the restart", func() bool {
		inst := w.instance()
		return inst.Restarts == 1 && inst.State == state.Running
	})
	if n := strings.Count(w.stderr.String(), "fakeserver: recv initialize"); n != 2 {
		t.Errorf("server saw initialize %d times, want 2 (original + replay)", n)
	}

	w.stdin.Close()
	if err := w.wait(5 * time.Second); err != nil {
		t.Fatalf("exit: %v\n%s", err, w.stderr)
	}
	if _, err := os.Stat(state.InstancePath(dir, "fake")); !os.IsNotExist(err) {
		t.Error("state file should be removed on clean shutdown")
	}
	if _, err := os.Stat(state.LogPath(dir, "fake")); err != nil {
		t.Errorf("log file missing: %v", err)
	}
}

func TestShutdownKillsGrandchildren(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "gc.pid")
	w := startWarden(t, dir, []string{"FAKE_GRANDCHILD_PIDFILE=" + pidfile})
	w.handshake()
	gc := readPID(t, pidfile)
	if !procscan.Alive(gc, time.Time{}) {
		t.Fatal("grandchild not running")
	}
	w.stdin.Close()
	if err := w.wait(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	eventually(t, "grandchild to die", func() bool { return !procscan.Alive(gc, time.Time{}) })
}

func TestHardKilledWardenLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "gc.pid")
	w := startWarden(t, dir, []string{"FAKE_GRANDCHILD_PIDFILE=" + pidfile})
	w.handshake()
	inst := w.instance()
	gc := readPID(t, pidfile)

	// No chance to clean up: TerminateProcess / SIGKILL.
	if err := w.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = w.cmd.Wait()

	if runtime.GOOS != "windows" {
		// Only Job Objects survive the supervisor's death. Elsewhere the
		// leftovers are found and cleaned by `reap`.
		var out bytes.Buffer
		code := cli.Main([]string{"reap", "--state-dir", dir}, cli.Env{Stdin: os.Stdin, Stdout: &out, Stderr: &out})
		if code != 0 {
			t.Fatalf("reap exited %d: %s", code, out.String())
		}
		t.Log(out.String())
	}
	eventually(t, "server to die", func() bool { return !procscan.Alive(inst.ChildPID, time.Time{}) })
	eventually(t, "grandchild to die", func() bool { return !procscan.Alive(gc, time.Time{}) })
}

func TestCircuitOpensOnCrashLoop(t *testing.T) {
	dir := t.TempDir()
	w := startWarden(t, dir, []string{"FAKE_CRASH_ON_START=1"}, "--max-restarts", "2")
	eventually(t, "circuit to open", func() bool {
		var inst state.Instance
		return state.ReadFile(state.InstancePath(dir, "fake"), &inst) == nil && inst.State == state.Failed
	})
	w.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	r := w.response(1)
	errObj, _ := r["error"].(map[string]any)
	data, _ := errObj["data"].(map[string]any)
	if data["reason"] != "circuit_open" {
		t.Errorf("response: %v", r)
	}
}

func TestCommandNotFound(t *testing.T) {
	cmd := exec.Command(wardenBin, "run", "--name", "nope", "--state-dir", t.TempDir(), "--", "definitely-not-a-real-binary-xyz")
	cmd.Stdin = strings.NewReader("")
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 127 {
		t.Errorf("exit = %v, want 127", err)
	}
}

func TestScanFindsAndKillsOrphan(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "orphan.pid")
	// The parent spawns a child and exits immediately: a textbook orphan.
	parent := exec.Command(fakeBin)
	parent.Env = append(os.Environ(), "FAKE_ORPHAN_PIDFILE="+pidfile)
	if err := parent.Run(); err != nil {
		t.Fatal(err)
	}
	orphan := readPID(t, pidfile)
	created, err := procscan.CreateTime(orphan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = procscan.Kill(orphan, created) })

	scan := func(args ...string) (string, int) {
		var out bytes.Buffer
		code := cli.Main(append([]string{"scan", "--state-dir", dir}, args...),
			cli.Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out})
		return out.String(), code
	}

	raw, code := scan("--json")
	if code != 0 {
		t.Fatalf("scan exited %d: %s", code, raw)
	}
	var s history.Scan
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	var found *classify.Tree
	for i, tr := range s.Result.Trees {
		if tr.Root.PID == orphan {
			found = &s.Result.Trees[i]
		}
	}
	if found == nil {
		t.Fatalf("orphan pid %d not found in scan", orphan)
	}
	if found.Class != classify.Orphaned {
		if runtime.GOOS == "linux" {
			// Containers may re-parent to a non-init subreaper (tini,
			// containerd-shim), which is legitimately "detached".
			t.Skipf("orphan classified %s (%s) in this environment", found.Class, found.Reason)
		}
		t.Fatalf("class = %s (%s), want orphaned", found.Class, found.Reason)
	}

	pidArg := "--pid=" + strconv.Itoa(orphan) // never touch real orphans on a dev machine
	if out, code := scan("--kill", pidArg); code == 0 || !strings.Contains(out, "--yes") {
		t.Errorf("--kill without --yes on a non-terminal must refuse; got %d: %s", code, out)
	}
	out, code := scan("--kill", "--yes", pidArg)
	if code != 0 {
		t.Fatalf("scan --kill exited %d: %s", code, out)
	}
	eventually(t, "orphan to die", func() bool { return !procscan.Alive(orphan, created) })

	events, _ := history.ReadEvents(dir)
	var types []string
	for _, e := range events {
		if e.PID == orphan {
			types = append(types, string(e.Type))
		}
	}
	if strings.Join(types, ",") != "appeared,killed" {
		t.Errorf("history for orphan = %v, want appeared,killed", types)
	}
}
