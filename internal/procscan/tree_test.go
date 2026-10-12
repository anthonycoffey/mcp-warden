package procscan

import (
	"slices"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

func pids(ps []Process) []int {
	out := make([]int, len(ps))
	for i, p := range ps {
		out[i] = p.PID
	}
	return out
}

func TestIndexTree(t *testing.T) {
	procs := []Process{
		{PID: 1, PPID: 0, CreateTime: at(0)},
		{PID: 10, PPID: 1, CreateTime: at(1)},
		{PID: 11, PPID: 10, CreateTime: at(2)},
		{PID: 12, PPID: 10, CreateTime: at(3)},
		{PID: 13, PPID: 11, CreateTime: at(4)},
		// PPID 99 does not exist: orphan.
		{PID: 20, PPID: 99, CreateTime: at(5)},
		// PPID 30 exists but was created after 21: PID reuse.
		{PID: 21, PPID: 30, CreateTime: at(6)},
		{PID: 30, PPID: 1, CreateTime: at(7)},
	}
	idx := NewIndex(procs)

	if got := pids(idx.Descendants(10)); !slices.Equal(got, []int{11, 12, 13}) {
		t.Errorf("Descendants(10) = %v", got)
	}
	if got := pids(idx.Ancestors(idx.ByPID[13])); !slices.Equal(got, []int{11, 10, 1}) {
		t.Errorf("Ancestors(13) = %v", got)
	}
	if _, ok := idx.Parent(idx.ByPID[20]); ok {
		t.Error("20's parent is dead")
	}
	if _, ok := idx.Parent(idx.ByPID[21]); ok {
		t.Error("21's parent PID was reused; must not link")
	}
	if got := pids(idx.Descendants(30)); len(got) != 0 {
		t.Errorf("impostor parent 30 must have no children, got %v", got)
	}
}

func TestAncestorsStopsOnCycle(t *testing.T) {
	idx := NewIndex([]Process{
		{PID: 5, PPID: 6, CreateTime: at(0)},
		{PID: 6, PPID: 5, CreateTime: at(0)},
	})
	if got := idx.Ancestors(idx.ByPID[5]); len(got) != 1 {
		t.Errorf("Ancestors = %v, want just [6]", pids(got))
	}
}

func TestGroupMembers(t *testing.T) {
	idx := NewIndex([]Process{
		{PID: 100, PGID: 100, CreateTime: at(10)}, // leader
		{PID: 101, PGID: 100, PPID: 1, CreateTime: at(11)},
		{PID: 50, PGID: 100, CreateTime: at(5)}, // older than the leader: a reused group id
		{PID: 102, PGID: 7, CreateTime: at(12)},
	})
	got := pids(idx.GroupMembers(100, at(10)))
	slices.Sort(got)
	if !slices.Equal(got, []int{100, 101}) {
		t.Errorf("GroupMembers = %v, want [100 101]", got)
	}
	if idx.GroupMembers(100, time.Time{}) != nil {
		t.Error("an unknown leader creation time must match nothing")
	}
}
