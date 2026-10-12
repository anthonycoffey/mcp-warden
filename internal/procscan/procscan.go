// Package procscan takes machine-wide process snapshots and kills processes
// safely. Each OS has its own implementation (Toolhelp + NtQueryInformation
// on Windows, /proc on Linux, sysctl on macOS); everything above this package
// works on the plain Process struct.
//
// Process identity is (PID, CreateTime), never PID alone: PIDs are reused,
// aggressively so on Windows, and a kill aimed at a reused PID would hit an
// unrelated process.
package procscan

import (
	"errors"
	"time"
)

// Process is one entry in a snapshot.
type Process struct {
	PID        int       `json:"pid"`
	PPID       int       `json:"ppid"`
	Name       string    `json:"name"`              // executable base name, e.g. "node.exe"
	Cmdline    string    `json:"cmdline,omitempty"` // full command line, best effort
	CreateTime time.Time `json:"createTime"`
	RSS        uint64    `json:"rss"` // resident memory in bytes; 0 if unknown

	// Unreadable is set when the OS denied access to the command line or
	// times (other users' or elevated processes). Such processes are never
	// classified or killed.
	Unreadable bool `json:"unreadable,omitempty"`
}

// ErrIdentityMismatch is returned by Kill when the PID now belongs to a
// different process than the one the caller observed.
var ErrIdentityMismatch = errors.New("procscan: pid was reused by another process")

// ErrNotFound is returned when the process no longer exists.
var ErrNotFound = errors.New("procscan: process not found")

// Snapshot returns every process visible to the current user.
func Snapshot() ([]Process, error) { return snapshot() }

// CreateTime returns the creation time of a live process, or ErrNotFound.
func CreateTime(pid int) (time.Time, error) { return createTime(pid) }

// Alive reports whether pid is running and, when created is non-zero, is
// still the same process that was created at that time.
func Alive(pid int, created time.Time) bool {
	if pid <= 0 {
		return false
	}
	ct, err := createTime(pid)
	if err != nil {
		return false
	}
	return created.IsZero() || SameTime(ct, created)
}

// Kill forcibly terminates pid. When created is non-zero, the process's
// identity is re-checked immediately before the kill and ErrIdentityMismatch
// is returned if it changed.
func Kill(pid int, created time.Time) error { return kill(pid, created) }

// SameTime compares creation times with a small tolerance, because some
// platforms derive them from clock ticks and they round-trip through JSON.
func SameTime(a, b time.Time) bool {
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	return d < 10*time.Millisecond
}
