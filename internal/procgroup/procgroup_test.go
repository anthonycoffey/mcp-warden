package procgroup

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anthonycoffey/mcp-warden/internal/procscan"
)

// The test binary doubles as the processes under test: with HELPER=parent it
// spawns a grandchild (HELPER=sleep), prints the grandchild's PID, and waits.
func TestMain(m *testing.M) {
	switch os.Getenv("PROCGROUP_HELPER") {
	case "parent":
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "PROCGROUP_HELPER=sleep")
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println(child.Process.Pid)
		time.Sleep(time.Minute)
		os.Exit(0)
	case "sleep":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// startTree starts the helper under a Group and returns the grandchild PID.
func startTree(t *testing.T) (*Group, *exec.Cmd, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "PROCGROUP_HELPER=parent")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	g, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if g.Degraded != nil {
		t.Logf("degraded mode: %v", g.Degraded)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("reading grandchild pid: %v", err)
	}
	gc, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	if !procscan.Alive(gc, time.Time{}) {
		t.Fatal("grandchild not running")
	}
	t.Cleanup(func() { _ = g.Kill(); _ = g.Close() })
	return g, cmd, gc
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !procscan.Alive(pid, time.Time{}) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive", pid)
}

func TestKillTerminatesGrandchildren(t *testing.T) {
	g, cmd, gc := startTree(t)
	if err := g.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waitDead(t, gc)
	if err := g.Kill(); err != nil {
		t.Errorf("second Kill = %v, want nil", err)
	}
}

func TestCloseKillsJobOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("kill-on-close is a Job Object feature")
	}
	g, cmd, gc := startTree(t)
	if g.Degraded != nil {
		t.Skip("job assignment unavailable in this environment")
	}
	// Closing the last handle is exactly what the kernel does when
	// mcp-warden is hard-killed.
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waitDead(t, gc)
}
