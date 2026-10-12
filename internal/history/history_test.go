package history

import (
	"os"
	"testing"
	"time"

	"github.com/anthonycoffey/mcp-warden/internal/classify"
	"github.com/anthonycoffey/mcp-warden/internal/procscan"
)

var t0 = time.Date(2026, 10, 12, 12, 0, 0, 0, time.Local)

func tree(pid int, class classify.Class, cmd string, rss uint64) classify.Tree {
	root := procscan.Process{PID: pid, Cmdline: cmd, CreateTime: t0}
	return classify.Tree{Key: classify.TreeKey(root), Class: class, Root: root,
		Members: []procscan.Process{root}, RSS: rss}
}

func TestDiff(t *testing.T) {
	a := tree(1, classify.Unmanaged, "npx a-mcp", 10)
	b := tree(2, classify.Orphaned, "npx b-mcp", 20)
	c := tree(3, classify.Detached, "npx c-mcp", 30)

	aOrphaned := a
	aOrphaned.Class = classify.Orphaned

	prev := Index([]classify.Tree{a, c})
	curr := Index([]classify.Tree{aOrphaned, b})
	events := Diff(prev, curr, t0)

	got := map[string]EventType{}
	for _, e := range events {
		got[e.Key] = e.Type
	}
	want := map[string]EventType{a.Key: Orphaned, b.Key: Appeared, c.Key: Disappeared}
	if len(got) != len(want) {
		t.Fatalf("events = %+v", events)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %s, want %s", k, got[k], v)
		}
	}
	if Diff(curr, curr, t0) != nil {
		t.Error("identical scans should produce no events")
	}
}

func TestWriteReadAcrossRotation(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := NewEvent(Appeared, tree(1, classify.Orphaned, "npx x-mcp", 1), t0)
	if err := w.Append([]Event{e, e}); err != nil {
		t.Fatal(err)
	}
	w.Close()
	// Simulate a rotated backup with an older event and a corrupt line.
	os.WriteFile(LogPath(dir)+".1", []byte(`{"time":"2026-10-11T00:00:00Z","type":"killed","key":"k"}`+"\nnot json\n"), 0o644)

	events, err := ReadEvents(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Type != Killed {
		t.Errorf("events = %+v", events)
	}
}

func TestSummarize(t *testing.T) {
	heroku := "npx -y @heroku/mcp-server"
	github := "npx -y @modelcontextprotocol/server-github"
	h1 := tree(10, classify.Orphaned, heroku, 100)
	h2 := tree(11, classify.Orphaned, heroku, 150)
	g1 := tree(20, classify.Unmanaged, github, 40)
	g1o := g1
	g1o.Class = classify.Orphaned

	events := []Event{
		NewEvent(Appeared, tree(1, classify.Orphaned, "ancient", 999), t0.Add(-48*time.Hour)), // before since
		NewEvent(Appeared, h1, t0),
		NewEvent(Appeared, g1, t0.Add(time.Minute)),      // not an orphan yet
		NewEvent(Orphaned, g1o, t0.Add(2*time.Minute)),   // now it is: live = 140
		NewEvent(Killed, h1, t0.Add(3*time.Minute)),      // live = 40
		NewEvent(Appeared, h2, t0.Add(24*time.Hour)),     // next day, live = 190 (peak)
		NewEvent(Disappeared, g1o, t0.Add(25*time.Hour)), // live = 150
		NewEvent(Appeared, tree(30, classify.Detached, "x", 5), t0.Add(26*time.Hour)),
	}
	s := Summarize(events, t0.Add(-time.Hour), 10)

	if s.Events != 7 {
		t.Errorf("events = %d, want 7 (one is before since)", s.Events)
	}
	if s.Orphans != 3 || s.Killed != 1 {
		t.Errorf("orphans=%d killed=%d", s.Orphans, s.Killed)
	}
	if s.PeakOrphanedRSS != 190 || !s.PeakAt.Equal(t0.Add(24*time.Hour)) {
		t.Errorf("peak = %d at %v", s.PeakOrphanedRSS, s.PeakAt)
	}
	if len(s.Days) != 2 || s.Days[0].Orphans != 2 || s.Days[1].Orphans != 1 {
		t.Errorf("days = %+v", s.Days)
	}
	if len(s.TopCommands) != 2 || s.TopCommands[0].Command != heroku ||
		s.TopCommands[0].Orphans != 2 || s.TopCommands[0].Killed != 1 || s.TopCommands[0].PeakRSS != 150 {
		t.Errorf("top = %+v", s.TopCommands)
	}
}
