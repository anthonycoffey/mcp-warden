// Package supervisor holds the restart policy: exponential backoff with
// jitter, and a sliding-window circuit breaker that gives up on a server
// that keeps crashing. Both are pure and clock-injected so they test
// deterministically.
package supervisor

import (
	"math"
	"math/rand/v2"
	"time"
)

// Backoff computes restart delays: Base * Factor^attempt, capped at Max, with
// up to ±Jitter (a fraction, e.g. 0.2) of random spread.
type Backoff struct {
	Base   time.Duration
	Max    time.Duration
	Factor float64
	Jitter float64

	// Rand returns a value in [0,1). Nil uses math/rand/v2.
	Rand func() float64
}

// DefaultBackoff is the policy from SPEC-001: 250ms base, doubling, 30s cap.
func DefaultBackoff() Backoff {
	return Backoff{Base: 250 * time.Millisecond, Max: 30 * time.Second, Factor: 2, Jitter: 0.2}
}

// Delay returns the wait before restart number attempt (0-based).
func (b Backoff) Delay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	factor := b.Factor
	if factor < 1 {
		factor = 1
	}
	d := float64(b.Base) * math.Pow(factor, float64(attempt))
	if b.Max > 0 && d > float64(b.Max) {
		d = float64(b.Max)
	}
	if b.Jitter > 0 {
		r := b.Rand
		if r == nil {
			r = rand.Float64
		}
		d += d * b.Jitter * (2*r() - 1)
	}
	if d < 0 {
		d = 0
	}
	return time.Duration(d)
}

// Breaker trips when more than Max restarts happen within Window.
type Breaker struct {
	Max    int
	Window time.Duration

	times []time.Time
}

// Record notes a crash at now and reports whether the breaker is now open.
func (b *Breaker) Record(now time.Time) (tripped bool) {
	b.prune(now)
	b.times = append(b.times, now)
	return len(b.times) > b.Max
}

// Recent returns how many crashes fall inside the window ending at now. It is
// used as the backoff attempt number, so a server that has been stable for a
// full window starts again from the base delay.
func (b *Breaker) Recent(now time.Time) int {
	b.prune(now)
	return len(b.times)
}

func (b *Breaker) prune(now time.Time) {
	cutoff := now.Add(-b.Window)
	i := 0
	for i < len(b.times) && !b.times[i].After(cutoff) {
		i++
	}
	b.times = b.times[i:]
}
