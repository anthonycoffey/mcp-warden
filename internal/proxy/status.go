package proxy

import (
	"sync"

	"github.com/anthonycoffey/mcp-warden/internal/state"
)

func (p *proxy) setPhase(ph state.Phase) {
	p.phase = ph
	if p.cfg.OnStatus == nil {
		return
	}
	s := Status{
		Phase:         ph,
		Restarts:      p.restarts,
		LastRestartAt: p.lastRestartAt,
		LastExit:      p.lastExit,
	}
	if p.child != nil {
		s.ChildPID = p.child.Pid()
		s.ChildStartedAt = p.childStarted
	}
	p.cfg.OnStatus(s)
}

// StatusRecorder is a concurrency-safe holder for the latest Status, for
// callers that publish it on their own schedule.
type StatusRecorder struct {
	mu sync.Mutex
	s  Status
	ch chan struct{}
}

// NewStatusRecorder returns a recorder; Changed fires (coalesced) on updates.
func NewStatusRecorder() *StatusRecorder {
	return &StatusRecorder{ch: make(chan struct{}, 1)}
}

// Set stores s. It never blocks, so it is safe to use as Config.OnStatus.
func (r *StatusRecorder) Set(s Status) {
	r.mu.Lock()
	r.s = s
	r.mu.Unlock()
	select {
	case r.ch <- struct{}{}:
	default:
	}
}

// Get returns the latest Status.
func (r *StatusRecorder) Get() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.s
}

// Changed signals after Set.
func (r *StatusRecorder) Changed() <-chan struct{} { return r.ch }
