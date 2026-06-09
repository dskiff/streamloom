package watcher

import (
	"fmt"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestRecord_CapsDistinctIPsPerStream(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	tr := NewTracker(clk)

	// Fill the stream to the per-stream cap with distinct IPs. The triple
	// (i>>16, i>>8, i) is unique for every i < 2^24, so all IPs are distinct.
	for i := 0; i < MaxIPsPerStream; i++ {
		tr.Record("s1", fmt.Sprintf("10.%d.%d.%d", i>>16&0xff, i>>8&0xff, i&0xff))
	}
	require.Equal(t, MaxIPsPerStream, tr.ActiveCount("s1", MaxWindowMs))

	// A previously-unseen IP past the cap is dropped, not inserted.
	tr.Record("s1", "203.0.113.1")
	assert.Equal(t, MaxIPsPerStream, tr.ActiveCount("s1", MaxWindowMs),
		"an unseen IP past the cap must be dropped")

	// An already-tracked IP still refreshes its last-seen even at capacity,
	// while a fresh unseen IP is still dropped.
	clk.Set(time.UnixMilli(5000))
	tr.Record("s1", "10.0.0.0")    // tracked above (i=0): refreshes to t=5000
	tr.Record("s1", "203.0.113.2") // unseen: dropped
	// A tight window isolates the refreshed entry; the dropped one never shows.
	assert.Equal(t, 1, tr.ActiveCount("s1", 1000),
		"tracked IPs must still refresh at capacity; dropped IPs must not appear")
}

func TestRecord_CapIsPerStream(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	tr := NewTracker(clk)

	// Saturating one stream must not affect another stream's tracking.
	for i := 0; i < MaxIPsPerStream; i++ {
		tr.Record("s1", fmt.Sprintf("10.%d.%d.%d", i>>16&0xff, i>>8&0xff, i&0xff))
	}
	tr.Record("s2", "203.0.113.1")
	assert.Equal(t, 1, tr.ActiveCount("s2", MaxWindowMs))
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
