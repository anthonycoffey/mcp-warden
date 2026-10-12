package procscan

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSnapshotFindsSelf(t *testing.T) {
	procs, err := Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) < 2 {
		t.Fatalf("only %d processes", len(procs))
	}
	idx := NewIndex(procs)
	self, ok := idx.ByPID[os.Getpid()]
	if !ok {
		t.Fatal("snapshot does not contain the test process")
	}
	if self.PPID != os.Getppid() {
		t.Errorf("PPID = %d, want %d", self.PPID, os.Getppid())
	}
	if self.Unreadable {
		t.Fatal("own process is unreadable")
	}
	if !strings.Contains(self.Cmdline, "procscan.test") {
		t.Errorf("cmdline = %q, expected the test binary", self.Cmdline)
	}
	if self.CreateTime.IsZero() || time.Since(self.CreateTime) > time.Hour {
		t.Errorf("create time = %v", self.CreateTime)
	}
	if runtime.GOOS != "darwin" && self.RSS == 0 {
		t.Error("RSS is zero")
	}
}

func TestAliveAndKill(t *testing.T) {
	cmd := sleeper(t)
	pid := cmd.Process.Pid

	ct, err := CreateTime(pid)
	if err != nil {
		t.Fatal(err)
	}
	if !Alive(pid, ct) {
		t.Fatal("child should be alive")
	}
	if Alive(pid, ct.Add(-time.Hour)) {
		t.Error("Alive must reject a mismatched create time")
	}
	if err := Kill(pid, ct.Add(-time.Hour)); !errors.Is(err, ErrIdentityMismatch) {
		t.Errorf("Kill with wrong identity = %v, want ErrIdentityMismatch", err)
	}
	if err := Kill(pid, ct); err != nil {
		t.Fatalf("Kill = %v", err)
	}
	_ = cmd.Wait()
	if Alive(pid, ct) {
		t.Error("child still alive after Kill")
	}
	if err := Kill(pid, ct); !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrIdentityMismatch) {
		t.Errorf("second Kill = %v, want ErrNotFound", err)
	}
}

// sleeper starts a long-lived child process that the test cleans up.
func sleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("powershell", "-NoProfile", "-Command", "Start-Sleep -Seconds 60")
	} else {
		cmd = exec.Command("sleep", "60")
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}
