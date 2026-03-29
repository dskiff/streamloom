package clock

import (
	"testing"
	"time"
)

func TestRealNow(t *testing.T) {
	before := time.Now()
	got := Real{}.Now()
	after := time.Now()

	if got.Before(before) || got.After(after) {
		t.Fatalf("Real.Now() = %v, want between %v and %v", got, before, after)
	}
}

func TestMock(t *testing.T) {
	fixed := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := NewMock(fixed)

	if got := clk.Now(); !got.Equal(fixed) {
		t.Fatalf("Mock.Now() = %v, want %v", got, fixed)
	}

	next := fixed.Add(5 * time.Second)
	clk.Set(next)

	if got := clk.Now(); !got.Equal(next) {
		t.Fatalf("after Set, Mock.Now() = %v, want %v", got, next)
	}
}

// Compile-time check that both types implement Clock.
var (
	_ Clock = Real{}
	_ Clock = (*Mock)(nil)
)

func TestRealNewTimer(t *testing.T) {
	timer := Real{}.NewTimer(10 * time.Millisecond)
	select {
	case <-timer.C():
	case <-time.After(1 * time.Second):
		t.Fatal("real timer did not fire within 1s")
	}
}

func TestMockTimer_FiresOnSet(t *testing.T) {
	clk := NewMock(time.UnixMilli(0))
	timer := clk.NewTimer(5 * time.Second)

	// Timer should not have fired yet.
	select {
	case <-timer.C():
		t.Fatal("timer fired before target time")
	default:
	}

	// Advance past the target.
	clk.Set(time.UnixMilli(6000))

	select {
	case <-timer.C():
	default:
		t.Fatal("timer did not fire after clock advanced past target")
	}
}

func TestMockTimer_ZeroDuration_FiresImmediately(t *testing.T) {
	clk := NewMock(time.UnixMilli(1000))
	timer := clk.NewTimer(0)

	select {
	case <-timer.C():
	default:
		t.Fatal("zero-duration timer did not fire immediately")
	}
}

func TestMockTimer_Stop_PreventsFireAndReturnsTrue(t *testing.T) {
	clk := NewMock(time.UnixMilli(0))
	timer := clk.NewTimer(5 * time.Second)

	if !timer.Stop() {
		t.Fatal("Stop() should return true for an active timer")
	}

	clk.Set(time.UnixMilli(10000))

	select {
	case <-timer.C():
		t.Fatal("stopped timer should not fire")
	default:
	}
}

func TestMockTimer_Stop_ReturnsFalseAfterFire(t *testing.T) {
	clk := NewMock(time.UnixMilli(0))
	timer := clk.NewTimer(5 * time.Second)
	clk.Set(time.UnixMilli(6000))

	if timer.Stop() {
		t.Fatal("Stop() should return false for a fired timer")
	}

	// Channel should still have the value (Stop does not drain).
	select {
	case <-timer.C():
	default:
		t.Fatal("channel should still have the fired value after Stop")
	}
}

func TestMockTimer_Reset(t *testing.T) {
	clk := NewMock(time.UnixMilli(0))
	timer := clk.NewTimer(5 * time.Second)

	// Stop and drain.
	timer.Stop()

	// Reset to 3 seconds from now (now=0, target=3000).
	timer.Reset(3 * time.Second)

	select {
	case <-timer.C():
		t.Fatal("reset timer should not fire before new target")
	default:
	}

	clk.Set(time.UnixMilli(4000))

	select {
	case <-timer.C():
	default:
		t.Fatal("reset timer should fire after new target reached")
	}
}

func TestMockTimer_Reset_ImmediateIfAlreadyPast(t *testing.T) {
	clk := NewMock(time.UnixMilli(10000))
	timer := clk.NewTimer(0)
	<-timer.C() // drain initial fire

	// Reset with 0 duration should fire immediately.
	timer.Reset(0)

	select {
	case <-timer.C():
	default:
		t.Fatal("reset with zero duration should fire immediately")
	}
}
