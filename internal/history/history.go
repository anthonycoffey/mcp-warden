// Package history turns successive scans into a change log
// (processes.jsonl) and summarizes that log to answer "which MCP server keeps
// leaking, and how much does it cost?".
package history

import (
	"bufio"
	"cmp"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/anthonycoffey/mcp-warden/internal/classify"
	"github.com/anthonycoffey/mcp-warden/internal/logfile"
	"github.com/anthonycoffey/mcp-warden/internal/state"
)

// EventType is the kind of change between two scans.
type EventType string

const (
	Appeared    EventType = "appeared"
	Orphaned    EventType = "orphaned" // an existing tree became an orphan
	Disappeared EventType = "disappeared"
	Killed      EventType = "killed"
)

// Event is one line of processes.jsonl.
type Event struct {
	Time    time.Time      `json:"time"`
	Type    EventType      `json:"type"`
	Key     string         `json:"key"`
	PID     int            `json:"pid"`
	Class   classify.Class `json:"class"`
	Client  string         `json:"client,omitempty"`
	Command string         `json:"command"`
	Procs   int            `json:"procs"`
	RSS     uint64         `json:"rss"`
}

// IsOrphaning reports whether e marks a tree becoming (or first seen as) an orphan.
func (e Event) IsOrphaning() bool {
	return e.Type == Orphaned || (e.Type == Appeared && e.Class == classify.Orphaned)
}

const maxCommand = 300

// NewEvent describes tree t.
func NewEvent(typ EventType, t classify.Tree, now time.Time) Event {
	cmd := t.Root.Cmdline
	if len(cmd) > maxCommand {
		cmd = cmd[:maxCommand] + "…"
	}
	return Event{Time: now, Type: typ, Key: t.Key, PID: t.Root.PID, Class: t.Class,
		Client: t.Client, Command: cmd, Procs: len(t.Members), RSS: t.RSS}
}

// Index keys trees by their stable identity.
func Index(trees []classify.Tree) map[string]classify.Tree {
	m := make(map[string]classify.Tree, len(trees))
	for _, t := range trees {
		m[t.Key] = t
	}
	return m
}

// Diff returns the events that turn prev into curr, in a deterministic order.
// Callers that kill a tree record a Killed event themselves and drop the tree
// from prev, so it is not also reported as Disappeared.
func Diff(prev, curr map[string]classify.Tree, now time.Time) []Event {
	var out []Event
	for key, t := range curr {
		p, existed := prev[key]
		switch {
		case !existed:
			out = append(out, NewEvent(Appeared, t, now))
		case p.Class != classify.Orphaned && t.Class == classify.Orphaned:
			out = append(out, NewEvent(Orphaned, t, now))
		}
	}
	for key, t := range prev {
		if _, ok := curr[key]; !ok {
			out = append(out, NewEvent(Disappeared, t, now))
		}
	}
	slices.SortFunc(out, func(a, b Event) int {
		return cmp.Or(cmp.Compare(a.Type, b.Type), cmp.Compare(a.Key, b.Key))
	})
	return out
}

// Paths

// LogPath is the event log under a state dir.
func LogPath(dir string) string { return filepath.Join(dir, "processes.jsonl") }

// ScanPath is the latest-snapshot file under a state dir.
func ScanPath(dir string) string { return filepath.Join(dir, "scan.json") }

// Writer appends events to the rotating log.
type Writer struct{ w *logfile.Writer }

// OpenWriter opens the event log under dir (10 MiB × 3 backups).
func OpenWriter(dir string) (*Writer, error) {
	w, err := logfile.Open(LogPath(dir), 0, 0)
	if err != nil {
		return nil, err
	}
	return &Writer{w: w}, nil
}

// Append writes events, one JSON object per line.
func (w *Writer) Append(events []Event) error {
	for _, e := range events {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err := w.w.Write(append(b, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// Close closes the log.
func (w *Writer) Close() error { return w.w.Close() }

// Scan is the latest snapshot, written by scan and watch.
type Scan struct {
	Time   time.Time       `json:"time"`
	Result classify.Result `json:"result"`
}

// SaveScan atomically writes the latest snapshot.
func SaveScan(dir string, s Scan) error { return state.WriteFile(ScanPath(dir), s) }

// LoadScan reads the latest snapshot.
func LoadScan(dir string) (Scan, error) {
	var s Scan
	err := state.ReadFile(ScanPath(dir), &s)
	return s, err
}

// ReadEvents reads all events from the log and its backups, oldest first.
// Malformed lines are skipped.
func ReadEvents(dir string) ([]Event, error) {
	var out []Event
	for _, path := range logfile.Files(LogPath(dir), 0) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var e Event
			if json.Unmarshal(sc.Bytes(), &e) == nil && e.Type != "" {
				out = append(out, e)
			}
		}
		f.Close()
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
