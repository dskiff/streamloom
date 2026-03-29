package clock

import (
	"sync"
	"time"
)

// Mock is a test clock whose time can be set atomically.
// When the time is advanced via Set, any mock timers whose target time has
// been reached are fired.
type Mock struct {
	mu     sync.Mutex
	t      time.Time
	timers []*mockTimer
}

// NewMock creates a Mock clock initialized to the given time.
func NewMock(t time.Time) *Mock {
	return &Mock{t: t}
}

// Now returns the time last set via Set (or the initial value).
func (c *Mock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Set updates the clock's time and fires any mock timers whose target time
// has been reached.
func (c *Mock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
	for _, mt := range c.timers {
		if mt.active && !t.Before(mt.target) {
			mt.active = false
			select {
			case mt.c <- t:
			default:
			}
		}
	}
}

// NewTimer creates a mock timer that fires when the mock clock reaches
// Now() + d. If d <= 0 the timer fires immediately.
func (c *Mock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()

	mt := &mockTimer{
		mock:   c,
		c:      make(chan time.Time, 1),
		target: c.t.Add(d),
		active: true,
	}
	c.timers = append(c.timers, mt)

	if d <= 0 {
		mt.active = false
		mt.c <- c.t
	}

	return mt
}

// mockTimer is a timer controlled by a Mock clock.
type mockTimer struct {
	mock   *Mock
	c      chan time.Time
	target time.Time
	active bool
}

func (mt *mockTimer) C() <-chan time.Time { return mt.c }

// Stop prevents the timer from firing. Returns true if the timer was active
// (i.e., had not yet fired or been stopped). As with time.Timer, the caller
// must drain C after a false return before calling Reset.
func (mt *mockTimer) Stop() bool {
	mt.mock.mu.Lock()
	defer mt.mock.mu.Unlock()
	was := mt.active
	mt.active = false
	return was
}

// Reset restarts the timer to fire after duration d from the mock clock's
// current time. The caller must have already stopped or drained the timer.
func (mt *mockTimer) Reset(d time.Duration) bool {
	mt.mock.mu.Lock()
	defer mt.mock.mu.Unlock()
	was := mt.active

	// Drain any pending value left from a previous cycle.
	select {
	case <-mt.c:
	default:
	}

	mt.target = mt.mock.t.Add(d)
	mt.active = true

	// Fire immediately if already past the target.
	if d <= 0 || !mt.mock.t.Before(mt.target) {
		mt.active = false
		select {
		case mt.c <- mt.mock.t:
		default:
		}
	}

	return was
}
