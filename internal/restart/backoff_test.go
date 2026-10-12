package restart

import (
	"testing"
	"time"
)

func TestBackoffDelay(t *testing.T) {
	b := Backoff{Base: 250 * time.Millisecond, Max: 30 * time.Second, Factor: 2}
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{-1, 250 * time.Millisecond},
		{0, 250 * time.Millisecond},
		{1, 500 * time.Millisecond},
		{2, time.Second},
		{6, 16 * time.Second},
		{7, 30 * time.Second}, // 32s capped
		{50, 30 * time.Second},
	}
	for _, tt := range tests {
		if got := b.Delay(tt.attempt); got != tt.want {
			t.Errorf("Delay(%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func TestBackoffJitterBounds(t *testing.T) {
	for _, r := range []float64{0, 0.5, 0.999} {
		b := Backoff{Base: time.Second, Factor: 2, Jitter: 0.2, Rand: func() float64 { return r }}
		got := b.Delay(0)
		if got < 800*time.Millisecond || got > 1200*time.Millisecond {
			t.Errorf("rand=%v: Delay = %v, outside ±20%%", r, got)
		}
	}
}

func TestBreaker(t *testing.T) {
	t0 := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	b := &Breaker{Max: 3, Window: time.Minute}

	for i := range 3 {
		if b.Record(t0.Add(time.Duration(i) * time.Second)) {
			t.Fatalf("tripped after %d crashes, max is 3", i+1)
		}
	}
	if !b.Record(t0.Add(4 * time.Second)) {
		t.Fatal("4th crash inside window should trip")
	}
}

func TestBreakerWindowSlides(t *testing.T) {
	t0 := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	b := &Breaker{Max: 2, Window: time.Minute}
	b.Record(t0)
	b.Record(t0.Add(10 * time.Second))
	// Well outside the window: old crashes are forgotten.
	if b.Record(t0.Add(5 * time.Minute)) {
		t.Fatal("crash after a quiet window should not trip")
	}
	if got := b.Recent(t0.Add(5 * time.Minute)); got != 1 {
		t.Errorf("Recent = %d, want 1", got)
	}
}
