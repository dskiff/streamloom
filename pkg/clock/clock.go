// Package clock provides a Clock interface and implementations for
// abstracting time access. All implementations are safe for concurrent use.
package clock

import "time"

// Timer abstracts time.Timer so that code using timers can be tested with
// a mock clock. The semantics mirror time.Timer: C returns the channel,
// Stop prevents firing (returning true if the timer was active), and Reset
// restarts the timer. As with time.Timer, callers must drain C after a
// false return from Stop before calling Reset.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

// Clock provides the current time and timer creation. Implementations
// must be safe for concurrent use.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Real uses the system clock.
type Real struct{}

// Now returns the current system time.
func (Real) Now() time.Time { return time.Now() }

// NewTimer returns a Timer backed by a real time.Timer.
func (Real) NewTimer(d time.Duration) Timer {
	return &realTimer{t: time.NewTimer(d)}
}

// realTimer wraps time.Timer to satisfy the Timer interface.
type realTimer struct{ t *time.Timer }

func (r *realTimer) C() <-chan time.Time        { return r.t.C }
func (r *realTimer) Stop() bool                 { return r.t.Stop() }
func (r *realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }
