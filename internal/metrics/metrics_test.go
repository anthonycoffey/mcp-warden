package metrics

import (
	"sync"
	"testing"
	"time"
)

func TestSnapshotPercentiles(t *testing.T) {
	m := New()
	for i := 1; i <= 100; i++ {
		m.Observe(time.Duration(i) * time.Millisecond)
	}
	s := m.Snapshot()
	if s.P50 != 50*time.Millisecond {
		t.Errorf("P50 = %v, want 50ms", s.P50)
	}
	if s.P95 != 95*time.Millisecond {
		t.Errorf("P95 = %v, want 95ms", s.P95)
	}
	if s.Max != 100*time.Millisecond {
		t.Errorf("Max = %v, want 100ms", s.Max)
	}
}

func TestEmptySnapshot(t *testing.T) {
	if s := New().Snapshot(); s != (Snapshot{}) {
		t.Errorf("empty snapshot = %+v", s)
	}
}

func TestReservoirKeepsRecent(t *testing.T) {
	m := New()
	for range reservoirSize {
		m.Observe(time.Hour)
	}
	for range reservoirSize {
		m.Observe(time.Millisecond)
	}
	s := m.Snapshot()
	if s.P95 != time.Millisecond {
		t.Errorf("P95 = %v; old samples should have been evicted", s.P95)
	}
	if s.Max != time.Hour {
		t.Errorf("Max = %v; max is all-time", s.Max)
	}
}

func TestConcurrentCounters(t *testing.T) {
	m := New()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				m.Request()
				m.Notification()
				m.Error()
				m.Observe(time.Microsecond)
			}
		})
	}
	wg.Wait()
	s := m.Snapshot()
	if s.Requests != 8000 || s.Notifications != 8000 || s.Errors != 8000 {
		t.Errorf("counters = %+v", s)
	}
}
