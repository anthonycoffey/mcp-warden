package state

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 12, 15, 0, 0, 123456789, time.UTC)
	in := Instance{
		Schema: SchemaVersion, Name: "github", Command: []string{"npx", "-y", "x"},
		WardenPID: 1, WardenCreatedAt: now, ChildPID: 2, State: Running, StartedAt: now,
		Counts: Counts{Requests: 3}, LatencyMs: Latency{P50: 4, P95: 5, Max: 6}, UpdatedAt: now,
	}
	if err := WriteFile(InstancePath(dir, "github"), in); err != nil {
		t.Fatal(err)
	}
	entries, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("List = %d entries", len(entries))
	}
	got := entries[0].Instance
	if got.Name != "github" || !got.WardenCreatedAt.Equal(now) || got.LatencyMs.P95 != 5 || got.State != Running {
		t.Errorf("round trip = %+v", got)
	}

	// No temp files left behind.
	files, _ := os.ReadDir(InstancesDir(dir))
	if len(files) != 1 {
		t.Errorf("instances dir has %d files", len(files))
	}
}

func TestConcurrentWritesAndReads(t *testing.T) {
	path := InstancePath(t.TempDir(), "x")
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 50 {
			if err := WriteFile(path, Instance{Schema: 1, Name: "x", Restarts: i}); err != nil {
				t.Error(err)
				return
			}
		}
	})
	wg.Go(func() {
		for range 50 {
			var inst Instance
			if err := ReadFile(path, &inst); err != nil && !os.IsNotExist(err) {
				t.Errorf("read saw a broken file: %v", err)
				return
			}
		}
	})
	wg.Wait()
}

func TestListSkipsJunk(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(InstancesDir(dir), 0o755)
	os.WriteFile(filepath.Join(InstancesDir(dir), "bad.json"), []byte("{nope"), 0o644)
	os.WriteFile(filepath.Join(InstancesDir(dir), "noschema.json"), []byte("{}"), 0o644)
	WriteFile(InstancePath(dir, "b"), Instance{Schema: 1, Name: "b"})
	WriteFile(InstancePath(dir, "a"), Instance{Schema: 1, Name: "a"})
	entries, _ := List(dir)
	if len(entries) != 2 || entries[0].Instance.Name != "a" {
		t.Errorf("List = %+v", entries)
	}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"github", "my_server-2", "a.b"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "../x", "a/b", `a\b`, ".hidden", "sp ace", string(make([]byte, 65))} {
		if ValidateName(bad) == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestDefaultDirOverride(t *testing.T) {
	t.Setenv("MCP_WARDEN_STATE_DIR", "/custom")
	if DefaultDir() != "/custom" {
		t.Error("env override ignored")
	}
}
