// Package state defines the per-instance state file that `mcp-warden run`
// writes and every read-side command (status, reap, scan) consumes. Files are
// replaced atomically, so readers never see a partial write.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// SchemaVersion is bumped on incompatible changes to Instance.
const SchemaVersion = 1

// Phase is the supervisor state of an instance.
type Phase string

const (
	Starting   Phase = "starting"
	Running    Phase = "running"
	Restarting Phase = "restarting"
	Failed     Phase = "failed"
	Stopped    Phase = "stopped"
)

// Latency holds request latency percentiles in milliseconds.
type Latency struct {
	P50 int64 `json:"p50"`
	P95 int64 `json:"p95"`
	Max int64 `json:"max"`
}

// Counts holds message counters.
type Counts struct {
	Requests      int64 `json:"requests"`
	Notifications int64 `json:"notifications"`
	Errors        int64 `json:"errors"`
}

// Instance is the on-disk record for one `mcp-warden run`.
type Instance struct {
	Schema          int       `json:"schema"`
	Name            string    `json:"name"`
	Command         []string  `json:"command"`
	WardenPID       int       `json:"wardenPid"`
	WardenCreatedAt time.Time `json:"wardenCreatedAt"`
	ChildPID        int       `json:"childPid,omitempty"`
	ChildStartedAt  time.Time `json:"childStartedAt,omitzero"`
	State           Phase     `json:"state"`
	StartedAt       time.Time `json:"startedAt"`
	LastRestartAt   time.Time `json:"lastRestartAt,omitzero"`
	Restarts        int       `json:"restarts"`
	LastExit        string    `json:"lastExit,omitempty"`
	Counts          Counts    `json:"counts"`
	LatencyMs       Latency   `json:"latencyMs"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

// DefaultDir returns the platform state directory, honoring
// MCP_WARDEN_STATE_DIR.
func DefaultDir() string {
	if d := os.Getenv("MCP_WARDEN_STATE_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "mcp-warden")
		}
		return filepath.Join(home, "AppData", "Local", "mcp-warden")
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "mcp-warden")
	default:
		if d := os.Getenv("XDG_STATE_HOME"); d != "" {
			return filepath.Join(d, "mcp-warden")
		}
		return filepath.Join(home, ".local", "state", "mcp-warden")
	}
}

// InstancesDir is where instance files live under a state dir.
func InstancesDir(dir string) string { return filepath.Join(dir, "instances") }

// LogPath is the log file for an instance name.
func LogPath(dir, name string) string { return filepath.Join(dir, "logs", name+".log") }

// InstancePath is the state file for an instance name.
func InstancePath(dir, name string) string {
	return filepath.Join(InstancesDir(dir), name+".json")
}

// ValidateName rejects names that would escape the state directory or be
// awkward on any filesystem.
func ValidateName(name string) error {
	if name == "" {
		return errors.New("name is required")
	}
	if len(name) > 64 {
		return errors.New("name must be at most 64 characters")
	}
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
		if !ok {
			return fmt.Errorf("name %q may only contain letters, digits, '-', '_' and '.'", name)
		}
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("name %q must not start with '.'", name)
	}
	return nil
}

// WriteFile atomically writes v as JSON to path: write a temp file in the same
// directory, then rename over the target.
func WriteFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(data, '\n'))
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return errors.Join(werr, cerr)
	}
	if err := renameRetry(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// ReadFile reads JSON from path into v, retrying briefly because on Windows a
// reader can collide with a concurrent rename.
func ReadFile(path string, v any) error {
	var err error
	for range 5 {
		var data []byte
		if data, err = os.ReadFile(path); err == nil {
			return json.Unmarshal(data, v)
		}
		if os.IsNotExist(err) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return err
}

func renameRetry(from, to string) error {
	var err error
	for range 5 {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return err
}

// Entry is an instance read back from disk.
type Entry struct {
	Path     string
	Instance Instance
}

// List reads every instance file in dir, sorted by name. Unreadable files are
// skipped.
func List(dir string) ([]Entry, error) {
	files, err := filepath.Glob(filepath.Join(InstancesDir(dir), "*.json"))
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, f := range files {
		var inst Instance
		if ReadFile(f, &inst) != nil || inst.Schema == 0 {
			continue
		}
		out = append(out, Entry{Path: f, Instance: inst})
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Instance.Name, b.Instance.Name) })
	return out, nil
}
