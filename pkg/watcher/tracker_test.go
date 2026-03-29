package watcher

import (
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/stretchr/testify/assert"
)

func TestRecord_NewStream(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	tr := NewTracker(clk)

	tr.Record("s1", "10.0.0.1")

	assert.Equal(t, 1, tr.ActiveCount("s1", 5000))
}

func TestRecord_UpdatesLastSeen(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	tr := NewTracker(clk)

	tr.Record("s1", "10.0.0.1")
	clk.Set(time.UnixMilli(5000))
	tr.Record("s1", "10.0.0.1")

	// The IP should still be active at time 5000 with a 1000ms window
	// because last-seen was updated to 5000.
	assert.Equal(t, 1, tr.ActiveCount("s1", 1000))
}

func TestActiveCount_WithinWindow(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(10000))
	tr := NewTracker(clk)

	tr.Record("s1", "10.0.0.1")
	clk.Set(time.UnixMilli(12000))

	// 10000 is within [12000-5000, 12000] = [7000, 12000]
	assert.Equal(t, 1, tr.ActiveCount("s1", 5000))
}

func TestActiveCount_OutsideWindow(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	tr := NewTracker(clk)

	tr.Record("s1", "10.0.0.1")
	clk.Set(time.UnixMilli(10000))

	// 1000 is outside [10000-2000, 10000] = [8000, 10000]
	assert.Equal(t, 0, tr.ActiveCount("s1", 2000))
}

func TestActiveCount_DistinctIPs(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	tr := NewTracker(clk)

	tr.Record("s1", "10.0.0.1")
	tr.Record("s1", "10.0.0.2")
	tr.Record("s1", "10.0.0.3")
	// Same IP again — should not increase count.
	tr.Record("s1", "10.0.0.1")

	assert.Equal(t, 3, tr.ActiveCount("s1", 5000))
}

func TestActiveCount_WindowCapped(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	tr := NewTracker(clk)

	tr.Record("s1", "10.0.0.1")

	// Advance past MaxWindowMs.
	clk.Set(time.UnixMilli(1000 + MaxWindowMs + 1))

	// Even with a huge window_ms, the cap at MaxWindowMs should exclude the entry.
	assert.Equal(t, 0, tr.ActiveCount("s1", MaxWindowMs*2))
}

func TestActiveCount_UnknownStream(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	tr := NewTracker(clk)

	assert.Equal(t, 0, tr.ActiveCount("nonexistent", 5000))
}

func TestActiveCount_DefaultWindow(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	tr := NewTracker(clk)

	tr.Record("s1", "10.0.0.1")

	// Advance just past DefaultWindowMs.
	clk.Set(time.UnixMilli(1000 + DefaultWindowMs + 1))

	// windowMs <= 0 should use DefaultWindowMs, so the entry is now outside.
	assert.Equal(t, 0, tr.ActiveCount("s1", 0))
	assert.Equal(t, 0, tr.ActiveCount("s1", -1))

	// But within the default window it should count.
	clk.Set(time.UnixMilli(1000 + DefaultWindowMs - 1))
	assert.Equal(t, 1, tr.ActiveCount("s1", 0))
}

func TestCleanup_RemovesOldEntries(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	tr := NewTracker(clk)

	tr.Record("s1", "10.0.0.1")
	tr.Record("s1", "10.0.0.2")

	// Advance past MaxWindowMs for the first record, but record the second IP again.
	clk.Set(time.UnixMilli(1000 + MaxWindowMs + 1))
	tr.Record("s1", "10.0.0.2")

	tr.Cleanup()

	// Only 10.0.0.2 should remain.
	assert.Equal(t, 1, tr.ActiveCount("s1", MaxWindowMs))
}

func TestCleanup_RemovesEmptyStreams(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	tr := NewTracker(clk)

	tr.Record("s1", "10.0.0.1")

	clk.Set(time.UnixMilli(1000 + MaxWindowMs + 1))
	tr.Cleanup()

	// Stream should be fully removed.
	tr.mu.RLock()
	_, exists := tr.streams["s1"]
	tr.mu.RUnlock()
	assert.False(t, exists)
}

func TestDeleteStream(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	tr := NewTracker(clk)

	tr.Record("s1", "10.0.0.1")
	tr.Record("s1", "10.0.0.2")

	tr.DeleteStream("s1")

	assert.Equal(t, 0, tr.ActiveCount("s1", MaxWindowMs))
}

func TestDeleteStream_NonExistent(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	tr := NewTracker(clk)

	// Should not panic.
	tr.DeleteStream("nonexistent")
}

func TestExtractIP(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want string
	}{
		{"ip with port", "192.168.1.1:8080", "192.168.1.1"},
		{"bare ip", "192.168.1.1", "192.168.1.1"},
		{"ipv6 with port", "[::1]:8080", "::1"},
		{"bare ipv6", "::1", "::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ExtractIP(tt.addr))
		})
	}
}
