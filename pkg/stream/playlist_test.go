package stream

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupStreamForPlaylist creates a Store, initializes a stream with the given
// target duration, and returns the Store and Stream. The clock is fixed to
// time zero (so all segment timestamps are in the future and CommitSlot accepts
// them). Use renderMediaPlaylist with a custom nowMs for filtering.
func setupStreamForPlaylist(t *testing.T, targetDurationSecs int) (*Store, *Stream) {
	t.Helper()

	clk := clock.NewMock(time.UnixMilli(0))

	store := NewStore(clk)
	meta := Metadata{
		Bandwidth:          4000000,
		Codecs:             "avc1.64001f",
		Width:              1920,
		Height:             1080,
		FrameRate:          23.976,
		TargetDurationSecs: targetDurationSecs,
	}
	err := store.Init("1", meta, []byte("init"), 0, 50, testSegmentBytes, 20, 5, 12)
	require.NoError(t, err)
	s := store.Get("1")
	require.NotNil(t, s)

	t.Cleanup(func() {
		store.Delete("1")
	})

	return store, s
}

func TestRenderMediaPlaylist_BasicWindow(t *testing.T) {
	// 5 segments, all eligible at nowMs=20000. Window=12 so all fit.
	_, s := setupStreamForPlaylist(t, 2)

	for i := range uint32(5) {
		mustCommitSlot(t, s, i, []byte("data"), int64(i)*2000, 2000)
	}

	s.mu.RLock()
	playlist, nextMs := s.renderMediaPlaylist(20000, 12)
	s.mu.RUnlock()

	assert.Equal(t, int64(0), nextMs, "no future segments")

	assert.True(t, strings.HasPrefix(playlist, "#EXTM3U\n"))
	assert.Contains(t, playlist, "#EXT-X-VERSION:7")
	assert.Contains(t, playlist, "#EXT-X-TARGETDURATION:2")
	assert.Contains(t, playlist, "#EXT-X-MEDIA-SEQUENCE:0")
	assert.Contains(t, playlist, "#EXT-X-MAP:URI=\"init_0.mp4\"")

	// All 5 segments should be present.
	for i := range 5 {
		assert.Contains(t, playlist, fmt.Sprintf("segment_%d.m4s", i))
		assert.Contains(t, playlist, "#EXTINF:2.000,")
	}
}

func TestRenderMediaPlaylist_SlidingWindow(t *testing.T) {
	// 15 segments, window=5. Only the last 5 should appear.
	_, s := setupStreamForPlaylist(t, 2)

	for i := range uint32(15) {
		mustCommitSlot(t, s, i, []byte("data"), int64(i)*2000, 2000)
	}

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(100000, 5)
	s.mu.RUnlock()

	// EXT-X-MEDIA-SEQUENCE should be 10 (first segment in window).
	assert.Contains(t, playlist, "#EXT-X-MEDIA-SEQUENCE:10")

	// Segments 0-9 should NOT appear.
	for i := range 10 {
		assert.NotContains(t, playlist, fmt.Sprintf("segment_%d.m4s", i))
	}
	// Segments 10-14 should appear.
	for i := 10; i < 15; i++ {
		assert.Contains(t, playlist, fmt.Sprintf("segment_%d.m4s", i))
	}
}

func TestRenderMediaPlaylist_NoEligibleSegments(t *testing.T) {
	// All segments are in the future relative to nowMs=0.
	_, s := setupStreamForPlaylist(t, 2)

	mustCommitSlot(t, s, 0, []byte("data"), 5000, 2000)

	s.mu.RLock()
	playlist, nextMs := s.renderMediaPlaylist(0, 12)
	s.mu.RUnlock()

	assert.Equal(t, "", playlist)
	assert.Equal(t, int64(5000), nextMs)
}

func TestRenderMediaPlaylist_EmptyStream(t *testing.T) {
	_, s := setupStreamForPlaylist(t, 2)

	s.mu.RLock()
	playlist, nextMs := s.renderMediaPlaylist(1000, 12)
	s.mu.RUnlock()

	assert.Equal(t, "", playlist)
	assert.Equal(t, int64(0), nextMs)
}

func TestRenderMediaPlaylist_SingleSegment(t *testing.T) {
	_, s := setupStreamForPlaylist(t, 4)

	mustCommitSlot(t, s, 42, []byte("data"), 5000, 4000)

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(10000, 12)
	s.mu.RUnlock()

	assert.Contains(t, playlist, "#EXT-X-MEDIA-SEQUENCE:42")
	assert.Contains(t, playlist, "segment_42.m4s")
	assert.Contains(t, playlist, "#EXTINF:4.000,")
	// No EXT-X-ENDLIST for live streams.
	assert.NotContains(t, playlist, "#EXT-X-ENDLIST")
}

func TestRenderMediaPlaylist_NonZeroStartingIndex(t *testing.T) {
	_, s := setupStreamForPlaylist(t, 2)

	// Indices starting at 100.
	for i := uint32(100); i < 105; i++ {
		mustCommitSlot(t, s, i, []byte("data"), int64(i-100)*2000, 2000)
	}

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(50000, 12)
	s.mu.RUnlock()

	assert.Contains(t, playlist, "#EXT-X-MEDIA-SEQUENCE:100")
}

func TestRenderMediaPlaylist_DurationFormatting(t *testing.T) {
	_, s := setupStreamForPlaylist(t, 3)

	// Various durations in ms.
	mustCommitSlot(t, s, 0, []byte("d"), 0, 2000)    // 2.000
	mustCommitSlot(t, s, 1, []byte("d"), 2000, 2500) // 2.500
	mustCommitSlot(t, s, 2, []byte("d"), 4500, 33)   // 0.033
	mustCommitSlot(t, s, 3, []byte("d"), 4533, 1001) // 1.001

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(50000, 12)
	s.mu.RUnlock()

	assert.Contains(t, playlist, "#EXTINF:2.000,")
	assert.Contains(t, playlist, "#EXTINF:2.500,")
	assert.Contains(t, playlist, "#EXTINF:0.033,")
	assert.Contains(t, playlist, "#EXTINF:1.001,")
}

func TestRenderMediaPlaylist_TimestampFormat(t *testing.T) {
	// Unix ms 1700000000000 = 2023-11-14T22:13:20.000Z
	_, s := setupStreamForPlaylist(t, 2)

	mustCommitSlot(t, s, 0, []byte("d"), 1700000000000, 2000)

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(1700000010000, 12)
	s.mu.RUnlock()

	assert.Contains(t, playlist, "#EXT-X-PROGRAM-DATE-TIME:2023-11-14T22:13:20.000Z")
}

func TestRenderMediaPlaylist_WallClockFiltering(t *testing.T) {
	// Mix of past and future segments relative to nowMs=10000.
	_, s := setupStreamForPlaylist(t, 2)

	mustCommitSlot(t, s, 0, []byte("d"), 2000, 2000)  // eligible
	mustCommitSlot(t, s, 1, []byte("d"), 4000, 2000)  // eligible
	mustCommitSlot(t, s, 2, []byte("d"), 10000, 2000) // eligible (at now, <= now)
	mustCommitSlot(t, s, 3, []byte("d"), 12000, 2000) // future
	mustCommitSlot(t, s, 4, []byte("d"), 14000, 2000) // future

	s.mu.RLock()
	playlist, nextMs := s.renderMediaPlaylist(10000, 12)
	s.mu.RUnlock()

	// Segments 0, 1, 2 should be present (timestamp <= 10000).
	assert.Contains(t, playlist, "segment_0.m4s")
	assert.Contains(t, playlist, "segment_1.m4s")
	assert.Contains(t, playlist, "segment_2.m4s")
	// Segments 3, 4 should NOT be present.
	assert.NotContains(t, playlist, "segment_3.m4s")
	assert.NotContains(t, playlist, "segment_4.m4s")

	assert.Equal(t, int64(12000), nextMs)
}

func TestRenderMediaPlaylist_NextEligibleMs(t *testing.T) {
	_, s := setupStreamForPlaylist(t, 2)

	mustCommitSlot(t, s, 0, []byte("d"), 1000, 2000)
	mustCommitSlot(t, s, 1, []byte("d"), 5000, 2000)
	mustCommitSlot(t, s, 2, []byte("d"), 9000, 2000)

	s.mu.RLock()
	_, nextMs := s.renderMediaPlaylist(6000, 12)
	s.mu.RUnlock()

	// Next segment not yet eligible is at 9000.
	assert.Equal(t, int64(9000), nextMs)
}

// --- Discontinuity tests ---

func TestRenderMediaPlaylist_SingleGeneration_NoDiscontinuity(t *testing.T) {
	_, s := setupStreamForPlaylist(t, 2)

	for i := range uint32(3) {
		mustCommitSlot(t, s, i, []byte("d"), int64(i)*2000, 2000)
	}

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(20000, 12)
	s.mu.RUnlock()

	// No discontinuity tags within a single generation.
	assert.NotContains(t, playlist, "#EXT-X-DISCONTINUITY\n")
	// Should have exactly one EXT-X-MAP for gen 0.
	assert.Equal(t, 1, strings.Count(playlist, "#EXT-X-MAP:URI="))
	assert.Contains(t, playlist, "#EXT-X-MAP:URI=\"init_0.mp4\"")
	// Discontinuity sequence should be 0.
	assert.Contains(t, playlist, "#EXT-X-DISCONTINUITY-SEQUENCE:0")
}

func TestRenderMediaPlaylist_TwoGenerations_Discontinuity(t *testing.T) {
	_, s := setupStreamForPlaylist(t, 2)

	require.NoError(t, s.AddInitEntry(1, []byte("init1")))

	// Gen 0 segments.
	mustCommitSlot(t, s, 0, []byte("d"), 0, 2000)
	mustCommitSlot(t, s, 1, []byte("d"), 2000, 2000)

	// Gen 1 segments (advances currentGeneration).
	err := commitSlotGen(t, s, 2, []byte("d"), 4000, 2000, 1)
	require.NoError(t, err)
	err = commitSlotGen(t, s, 3, []byte("d"), 6000, 2000, 1)
	require.NoError(t, err)

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(20000, 12)
	s.mu.RUnlock()

	// Should have exactly one #EXT-X-DISCONTINUITY (between gen 0 and gen 1).
	assert.Equal(t, 1, strings.Count(playlist, "#EXT-X-DISCONTINUITY\n"))
	// Should have two EXT-X-MAP entries.
	assert.Equal(t, 2, strings.Count(playlist, "#EXT-X-MAP:URI="))
	assert.Contains(t, playlist, "#EXT-X-MAP:URI=\"init_0.mp4\"")
	assert.Contains(t, playlist, "#EXT-X-MAP:URI=\"init_1.mp4\"")
	// Discontinuity sequence still 0 (nothing scrolled out).
	assert.Contains(t, playlist, "#EXT-X-DISCONTINUITY-SEQUENCE:0")
	// All 4 segments present.
	for i := range 4 {
		assert.Contains(t, playlist, fmt.Sprintf("segment_%d.m4s", i))
	}
}

func TestRenderMediaPlaylist_ThreeGenerations(t *testing.T) {
	_, s := setupStreamForPlaylist(t, 2)

	require.NoError(t, s.AddInitEntry(1, []byte("init1")))
	require.NoError(t, s.AddInitEntry(2, []byte("init2")))

	mustCommitSlot(t, s, 0, []byte("d"), 0, 2000)
	err := commitSlotGen(t, s, 1, []byte("d"), 2000, 2000, 1)
	require.NoError(t, err)
	err = commitSlotGen(t, s, 2, []byte("d"), 4000, 2000, 2)
	require.NoError(t, err)

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(20000, 12)
	s.mu.RUnlock()

	// Two discontinuities (0→1, 1→2).
	assert.Equal(t, 2, strings.Count(playlist, "#EXT-X-DISCONTINUITY\n"))
	// Three MAP entries.
	assert.Equal(t, 3, strings.Count(playlist, "#EXT-X-MAP:URI="))
}

func TestRenderMediaPlaylist_DiscontinuitySequence_AfterEviction(t *testing.T) {
	// Use a stream with backwardBufferSize=1 so eviction happens aggressively.
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{
		Bandwidth:          4000000,
		Codecs:             "avc1.64001f",
		Width:              1920,
		Height:             1080,
		FrameRate:          23.976,
		TargetDurationSecs: 2,
	}
	err := store.Init("1", meta, []byte("init0"), 0, 50, testSegmentBytes, 1, 5, 3)
	require.NoError(t, err)
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")
	require.NotNil(t, s)

	require.NoError(t, s.AddInitEntry(1, []byte("init1")))

	// Push gen=0 segments at timestamps 1000, 3000, 5000.
	mustCommitSlot(t, s, 0, []byte("d"), 1000, 2000)
	mustCommitSlot(t, s, 1, []byte("d"), 3000, 2000)
	mustCommitSlot(t, s, 2, []byte("d"), 5000, 2000)

	// Push gen=1 segments at 7000, 9000.
	err = commitSlotGen(t, s, 3, []byte("d"), 7000, 2000, 1)
	require.NoError(t, err)
	err = commitSlotGen(t, s, 4, []byte("d"), 9000, 2000, 1)
	require.NoError(t, err)

	// Advance clock to 10000. Segments 0,1,2,3 are in the past.
	// backwardBufferSize=1, so eviction should remove the oldest past segments.
	// CommitSlot triggers eviction, so push one more to trigger it.
	clk.Set(time.UnixMilli(10000))
	err = commitSlotGen(t, s, 5, []byte("d"), 11000, 2000, 1)
	require.NoError(t, err)

	// Render with window=3, nowMs=11000.
	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(11000, 3)
	s.mu.RUnlock()

	// The eviction should have removed segments 0,1,2 (gen=0) and possibly 3 (gen=1).
	// The discontinuity (gen 0→1) should have scrolled out, so DISCONTINUITY-SEQUENCE >= 1.
	assert.Contains(t, playlist, "#EXT-X-DISCONTINUITY-SEQUENCE:1")
}

func TestRenderMediaPlaylist_DiscontinuitySequence_AllPreWindowEvicted(t *testing.T) {
	// Regression test: when eviction removes ALL pre-window segments, start == 0
	// and the boundary discontinuity between the last evicted segment and
	// segments[0] must still be counted in DISCONTINUITY-SEQUENCE.
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{
		Bandwidth:          4000000,
		Codecs:             "avc1.64001f",
		Width:              1920,
		Height:             1080,
		FrameRate:          23.976,
		TargetDurationSecs: 2,
	}
	// backwardBufferSize=1, windowSize=3
	err := store.Init("1", meta, []byte("init0"), 0, 50, testSegmentBytes, 1, 5, 3)
	require.NoError(t, err)
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")
	require.NotNil(t, s)

	require.NoError(t, s.AddInitEntry(1, []byte("init1")))

	// Push 2 gen=0 segments.
	mustCommitSlot(t, s, 0, []byte("d"), 1000, 2000)
	mustCommitSlot(t, s, 1, []byte("d"), 3000, 2000)

	// Push 3 gen=1 segments.
	require.NoError(t, commitSlotGen(t, s, 2, []byte("d"), 5000, 2000, 1))
	require.NoError(t, commitSlotGen(t, s, 3, []byte("d"), 7000, 2000, 1))
	require.NoError(t, commitSlotGen(t, s, 4, []byte("d"), 9000, 2000, 1))

	// Advance clock so all 5 segments are in the past, then push one more
	// gen=1 segment to trigger eviction.
	clk.Set(time.UnixMilli(10000))
	require.NoError(t, commitSlotGen(t, s, 5, []byte("d"), 11000, 2000, 1))

	// backwardBufferSize=1 means eviction keeps only 1 past segment.
	// 5 segments are in the past (ts <= 10000), so 4 are evicted (indices 0-3).
	// Remaining: index 4 (past, kept as backward buffer), index 5 (future).
	// Window of 3 at nowMs=11000: eligible segments are those with ts <= 11000,
	// which is all 2 remaining. start = max(2-3, 0) = 0.
	//
	// The gen 0→1 boundary was between evicted index 1 (gen 0) and evicted
	// index 2 (gen 1). evictedDiscontinuities captured that. But the boundary
	// between the last evicted segment (index 3, gen 1) and segments[0]
	// (index 4, gen 1) has no transition, so no extra increment.
	//
	// To test the start==0 boundary case, we need the last evicted segment
	// to differ from segments[0]. Let's set up that scenario directly.

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(11000, 3)
	s.mu.RUnlock()

	// The gen 0→1 transition was among the evicted segments.
	assert.Contains(t, playlist, "#EXT-X-DISCONTINUITY-SEQUENCE:1",
		"evicted gen 0→1 transition should be counted; got:\n%s", playlist)

	// No discontinuity within the window (all remaining segments are gen 1).
	assert.NotContains(t, playlist, "#EXT-X-DISCONTINUITY\n",
		"no in-window discontinuity expected; got:\n%s", playlist)
}

func TestRenderMediaPlaylist_DiscontinuitySequence_EvictedBoundaryAtStartZero(t *testing.T) {
	// Specific test for the boundary between lastEvictedGeneration and
	// segments[0] when start == 0: all pre-window segments have been evicted,
	// and the last evicted segment's generation differs from segments[0].
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{
		Bandwidth:          4000000,
		Codecs:             "avc1.64001f",
		Width:              1920,
		Height:             1080,
		FrameRate:          23.976,
		TargetDurationSecs: 2,
	}
	// backwardBufferSize=1, windowSize=2
	err := store.Init("1", meta, []byte("init0"), 0, 50, testSegmentBytes, 1, 5, 2)
	require.NoError(t, err)
	t.Cleanup(func() { store.Delete("1") })
	s := store.Get("1")
	require.NotNil(t, s)

	require.NoError(t, s.AddInitEntry(1, []byte("init1")))

	// Push 1 gen=0 segment.
	mustCommitSlot(t, s, 0, []byte("d"), 1000, 2000)

	// Push 2 gen=1 segments.
	require.NoError(t, commitSlotGen(t, s, 1, []byte("d"), 3000, 2000, 1))
	require.NoError(t, commitSlotGen(t, s, 2, []byte("d"), 5000, 2000, 1))

	// Advance clock so segments 0,1,2 are in the past.
	clk.Set(time.UnixMilli(6000))
	// Push gen=1 segment to trigger eviction.
	require.NoError(t, commitSlotGen(t, s, 3, []byte("d"), 7000, 2000, 1))

	// backwardBufferSize=1: 3 past segments (0,1,2), evict 2 → indices 0,1 evicted.
	// Remaining: [2(gen1), 3(gen1)]. start=0 (window=2 fits all 2 eligible).
	// lastEvictedGeneration = gen1 (index 1 was gen1).
	// But evictedDiscontinuities should be 1 (index 0 was gen0, index 1 was gen1).
	// Boundary: lastEvicted gen1 vs segments[0] gen1 → no extra increment.
	// Total discSeq = 1. ✓

	// Now advance further and push more to evict index 2 (gen1).
	clk.Set(time.UnixMilli(8000))
	require.NoError(t, commitSlotGen(t, s, 4, []byte("d"), 9000, 2000, 1))

	require.NoError(t, s.AddInitEntry(2, []byte("init2")))
	require.NoError(t, commitSlotGen(t, s, 5, []byte("d"), 11000, 2000, 2))

	// Now advance and evict so the gen1→gen2 boundary is at start==0.
	clk.Set(time.UnixMilli(12000))
	require.NoError(t, commitSlotGen(t, s, 6, []byte("d"), 13000, 2000, 2))

	// Render: window=2 at nowMs=13000.
	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(13000, 2)
	s.mu.RUnlock()

	// The gen0→1 and gen1→2 transitions should both be in DISCONTINUITY-SEQUENCE.
	// The window contains only gen=2 segments, so no in-window discontinuity.
	assert.Contains(t, playlist, "#EXT-X-DISCONTINUITY-SEQUENCE:2",
		"both scrolled-out transitions should be counted; got:\n%s", playlist)
	assert.NotContains(t, playlist, "#EXT-X-DISCONTINUITY\n",
		"no in-window discontinuity expected; got:\n%s", playlist)
}

func TestRenderMediaPlaylist_DiscontinuitySequence_WindowBoundary(t *testing.T) {
	// Regression test: when windowing (not eviction) pushes a generation
	// transition out of the playlist window, the transition at
	// segments[start-1] → segments[start] must be counted.
	_, s := setupStreamForPlaylist(t, 2)

	require.NoError(t, s.AddInitEntry(1, []byte("init1")))

	// Push 2 gen=0 segments, then 2 gen=1 segments.
	mustCommitSlot(t, s, 0, []byte("d"), 0, 2000)
	mustCommitSlot(t, s, 1, []byte("d"), 2000, 2000)
	require.NoError(t, commitSlotGen(t, s, 2, []byte("d"), 4000, 2000, 1))
	require.NoError(t, commitSlotGen(t, s, 3, []byte("d"), 6000, 2000, 1))

	// Render with windowSize=2 so only segments 2,3 (gen=1) are in the window.
	// The gen 0→1 transition at the window boundary must be counted.
	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(20000, 2)
	s.mu.RUnlock()

	assert.Contains(t, playlist, "#EXT-X-DISCONTINUITY-SEQUENCE:1",
		"window-boundary transition should be counted; got:\n%s", playlist)
	assert.NotContains(t, playlist, "#EXT-X-DISCONTINUITY\n",
		"no in-window discontinuity expected; got:\n%s", playlist)
	assert.Contains(t, playlist, `#EXT-X-MAP:URI="init_1.mp4"`)
	assert.NotContains(t, playlist, `#EXT-X-MAP:URI="init_0.mp4"`)
}

func TestRenderMediaPlaylist_DiscontinuitySequence_WindowBoundaryMultiple(t *testing.T) {
	// Multiple transitions scroll out via windowing alone (no eviction).
	_, s := setupStreamForPlaylist(t, 2)

	require.NoError(t, s.AddInitEntry(1, []byte("init1")))
	require.NoError(t, s.AddInitEntry(2, []byte("init2")))

	// gen=0, gen=1, gen=2, gen=2 — window=2 shows only the last 2 (gen=2).
	mustCommitSlot(t, s, 0, []byte("d"), 0, 2000)
	require.NoError(t, commitSlotGen(t, s, 1, []byte("d"), 2000, 2000, 1))
	require.NoError(t, commitSlotGen(t, s, 2, []byte("d"), 4000, 2000, 2))
	require.NoError(t, commitSlotGen(t, s, 3, []byte("d"), 6000, 2000, 2))

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(20000, 2)
	s.mu.RUnlock()

	// Both 0→1 and 1→2 transitions scrolled out.
	assert.Contains(t, playlist, "#EXT-X-DISCONTINUITY-SEQUENCE:2",
		"both transitions should be counted; got:\n%s", playlist)
}

func TestRenderMediaPlaylist_DiscontinuityOrder(t *testing.T) {
	// Verify the exact ordering: DISCONTINUITY comes before MAP for new generation.
	_, s := setupStreamForPlaylist(t, 2)

	require.NoError(t, s.AddInitEntry(1, []byte("init1")))

	mustCommitSlot(t, s, 0, []byte("d"), 0, 2000)
	err := commitSlotGen(t, s, 1, []byte("d"), 2000, 2000, 1)
	require.NoError(t, err)

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(20000, 12)
	s.mu.RUnlock()

	// The DISCONTINUITY tag must come before the MAP for gen 1.
	discIdx := strings.Index(playlist, "#EXT-X-DISCONTINUITY\n")
	mapGen1Idx := strings.Index(playlist, "#EXT-X-MAP:URI=\"init_1.mp4\"")
	assert.Greater(t, discIdx, 0, "DISCONTINUITY should be present")
	assert.Greater(t, mapGen1Idx, discIdx, "MAP for gen 1 should come after DISCONTINUITY")

	// The first MAP (gen 0) should come before the DISCONTINUITY.
	mapGen0Idx := strings.Index(playlist, "#EXT-X-MAP:URI=\"init_0.mp4\"")
	assert.Less(t, mapGen0Idx, discIdx, "MAP for gen 0 should come before DISCONTINUITY")
}

func TestCommitSlot_MissingInitForGeneration(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	mustInit(t, store, "g", meta, []byte("init"), 10, testSegmentBytes, 5)
	s := store.Get("g")

	// Pushing a segment at generation 5 without an init entry should fail.
	err := commitSlotGen(t, s, 0, []byte("data"), 5000, 2000, 5)
	assert.ErrorIs(t, err, ErrMissingInitForGeneration)
}

func TestAddInitEntry_Validation(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	mustInit(t, store, "g", meta, []byte("init"), 10, testSegmentBytes, 5)
	s := store.Get("g")

	// Negative generation should fail.
	err := s.AddInitEntry(-1, []byte("init"))
	assert.ErrorIs(t, err, ErrNegativeGeneration)

	// Empty init data should fail.
	err = s.AddInitEntry(1, []byte{})
	assert.ErrorIs(t, err, ErrEmptyInitData)

	// Generation 0 already has an init entry (created by Init) → duplicate.
	err = s.AddInitEntry(0, []byte("init-dup"))
	assert.ErrorIs(t, err, ErrDuplicateGeneration)

	// Adding generation 1 (> max existing gen 0) should succeed.
	err = s.AddInitEntry(1, []byte("init1"))
	assert.NoError(t, err)

	// Adding generation 1 again → duplicate.
	err = s.AddInitEntry(1, []byte("init1-dup"))
	assert.ErrorIs(t, err, ErrDuplicateGeneration)

	// Adding generation 3 (skipping 2) should succeed.
	err = s.AddInitEntry(3, []byte("init3"))
	assert.NoError(t, err)

	// Adding generation 2 (less than max existing 3) → not monotonic.
	err = s.AddInitEntry(2, []byte("init2"))
	assert.ErrorIs(t, err, ErrGenerationNotMonotonic)
}

func TestAddInitEntry_ClonesData(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	mustInit(t, store, "g", meta, []byte("init"), 10, testSegmentBytes, 5)
	s := store.Get("g")

	data := []byte{0x01, 0x02, 0x03}
	require.NoError(t, s.AddInitEntry(1, data))

	// Mutate the caller's slice.
	data[0] = 0xFF

	entry, ok := s.GetInitEntry(1)
	require.True(t, ok)
	assert.Equal(t, byte(0x01), entry.InitData[0], "AddInitEntry should clone input")
}
