package procscan

import (
	"errors"
	"time"
)

// Index is a snapshot organized for parent/child lookups.
type Index struct {
	ByPID    map[int]Process
	children map[int][]int
}

// NewIndex builds an Index. A process is only linked to its parent when the
// parent was created no later than the child; otherwise the PPID refers to a
// dead parent whose PID has since been reused.
func NewIndex(procs []Process) *Index {
	idx := &Index{ByPID: make(map[int]Process, len(procs)), children: make(map[int][]int)}
	for _, p := range procs {
		idx.ByPID[p.PID] = p
	}
	for _, p := range procs {
		if parent, ok := idx.Parent(p); ok {
			idx.children[parent.PID] = append(idx.children[parent.PID], p.PID)
		}
	}
	return idx
}

// Parent returns p's live parent, if it has one. A parent whose creation time
// is after the child's is a PID-reuse impostor and is not returned.
func (idx *Index) Parent(p Process) (Process, bool) {
	if p.PPID <= 0 || p.PPID == p.PID {
		return Process{}, false
	}
	parent, ok := idx.ByPID[p.PPID]
	if !ok {
		return Process{}, false
	}
	if !parent.CreateTime.IsZero() && !p.CreateTime.IsZero() &&
		parent.CreateTime.After(p.CreateTime.Add(time.Millisecond)) {
		return Process{}, false
	}
	return parent, true
}

// Ancestors returns p's chain of live ancestors, nearest first. It stops at
// cycles, which can appear in a racy snapshot.
func (idx *Index) Ancestors(p Process) []Process {
	var out []Process
	seen := map[int]bool{p.PID: true}
	for {
		parent, ok := idx.Parent(p)
		if !ok || seen[parent.PID] {
			return out
		}
		seen[parent.PID] = true
		out = append(out, parent)
		p = parent
	}
}

// Descendants returns every live descendant of pid (not including pid),
// breadth-first: parents always appear before their children.
func (idx *Index) Descendants(pid int) []Process {
	var out []Process
	seen := map[int]bool{pid: true}
	queue := []int{pid}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range idx.children[cur] {
			if seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, idx.ByPID[c])
			queue = append(queue, c)
		}
	}
	return out
}

// KillTree kills root and all of its descendants, deepest first so that no
// parent gets a chance to respawn a child. It returns the processes it
// killed; identity mismatches and already-exited processes are skipped.
func KillTree(idx *Index, root Process) ([]Process, error) {
	victims := append([]Process{root}, idx.Descendants(root.PID)...)
	var killed []Process
	var firstErr error
	for i := len(victims) - 1; i >= 0; i-- {
		v := victims[i]
		err := Kill(v.PID, v.CreateTime)
		switch {
		case err == nil:
			killed = append(killed, v)
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrIdentityMismatch):
		default:
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return killed, firstErr
}
