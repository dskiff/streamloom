package stream

import (
	"bytes"
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
	assert.Contains(t, playlist, "#EXT-X-DISCONTINUITY-SEQUENCE:0\n")
}

func TestRenderMediaPlaylist_TwoGenerations_Discontinuity(t *testing.T) {
	_, s := setupStreamForPlaylist(t, 2)

	mustAddInit(t, s, 1, []byte("init1"))

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
	assert.Contains(t, playlist, "#EXT-X-DISCONTINUITY-SEQUENCE:0\n")
	// All 4 segments present.
	for i := range 4 {
		assert.Contains(t, playlist, fmt.Sprintf("segment_%d.m4s", i))
	}
}

func TestRenderMediaPlaylist_ThreeGenerations(t *testing.T) {
	_, s := setupStreamForPlaylist(t, 2)

	mustAddInit(t, s, 1, []byte("init1"))
	mustAddInit(t, s, 2, []byte("init2"))

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

	mustAddInit(t, s, 1, []byte("init1"))

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
	assert.Contains(t, playlist, "#EXT-X-DISCONTINUITY-SEQUENCE:1\n")
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

	mustAddInit(t, s, 1, []byte("init1"))

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
	// index 2 (gen 1). evictedDiscontinuities captured that. The boundary
	// between the last evicted segment (index 3, gen 1) and segments[0]
	// (index 4, gen 1) has no transition, so no extra increment.
	// The case where lastEvictedGeneration differs from segments[0] is
	// covered by TestRenderMediaPlaylist_DiscontinuitySequence_EvictedBoundaryAtStartZero.

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(11000, 3)
	s.mu.RUnlock()

	// The gen 0→1 transition was among the evicted segments.
	assert.Contains(t, playlist, "#EXT-X-DISCONTINUITY-SEQUENCE:1\n",
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

	mustAddInit(t, s, 1, []byte("init1"))

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

	mustAddInit(t, s, 2, []byte("init2"))
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
	assert.Contains(t, playlist, "#EXT-X-DISCONTINUITY-SEQUENCE:2\n",
		"both scrolled-out transitions should be counted; got:\n%s", playlist)
	assert.NotContains(t, playlist, "#EXT-X-DISCONTINUITY\n",
		"no in-window discontinuity expected; got:\n%s", playlist)
}

func TestRenderMediaPlaylist_DiscontinuitySequence_WindowBoundary(t *testing.T) {
	// Regression test: when windowing (not eviction) pushes a generation
	// transition out of the playlist window, the transition at
	// segments[start-1] → segments[start] must be counted.
	_, s := setupStreamForPlaylist(t, 2)

	mustAddInit(t, s, 1, []byte("init1"))

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

	assert.Contains(t, playlist, "#EXT-X-DISCONTINUITY-SEQUENCE:1\n",
		"window-boundary transition should be counted; got:\n%s", playlist)
	assert.NotContains(t, playlist, "#EXT-X-DISCONTINUITY\n",
		"no in-window discontinuity expected; got:\n%s", playlist)
	assert.Contains(t, playlist, `#EXT-X-MAP:URI="init_1.mp4"`)
	assert.NotContains(t, playlist, `#EXT-X-MAP:URI="init_0.mp4"`)
}

func TestRenderMediaPlaylist_DiscontinuitySequence_WindowBoundaryMultiple(t *testing.T) {
	// Multiple transitions scroll out via windowing alone (no eviction).
	_, s := setupStreamForPlaylist(t, 2)

	mustAddInit(t, s, 1, []byte("init1"))
	mustAddInit(t, s, 2, []byte("init2"))

	// gen=0, gen=1, gen=2, gen=2 — window=2 shows only the last 2 (gen=2).
	mustCommitSlot(t, s, 0, []byte("d"), 0, 2000)
	require.NoError(t, commitSlotGen(t, s, 1, []byte("d"), 2000, 2000, 1))
	require.NoError(t, commitSlotGen(t, s, 2, []byte("d"), 4000, 2000, 2))
	require.NoError(t, commitSlotGen(t, s, 3, []byte("d"), 6000, 2000, 2))

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(20000, 2)
	s.mu.RUnlock()

	// Both 0→1 and 1→2 transitions scrolled out.
	assert.Contains(t, playlist, "#EXT-X-DISCONTINUITY-SEQUENCE:2\n",
		"both transitions should be counted; got:\n%s", playlist)
}

func TestRenderMediaPlaylist_SkippedGeneration(t *testing.T) {
	// Init entries exist for gen 0, 1, and 2, but segments exist only at
	// gen 0 and gen 2 (gen 1 is skipped). The playlist should show a
	// discontinuity between gen 0 and gen 2, and MAP should reference
	// init_2.mp4 (not init_1.mp4).
	_, s := setupStreamForPlaylist(t, 2)

	mustAddInit(t, s, 1, []byte("init1"))
	mustAddInit(t, s, 2, []byte("init2"))

	// Gen 0 segments.
	mustCommitSlot(t, s, 0, []byte("d"), 0, 2000)
	mustCommitSlot(t, s, 1, []byte("d"), 2000, 2000)

	// Skip gen 1 entirely — go straight to gen 2.
	require.NoError(t, commitSlotGen(t, s, 2, []byte("d"), 4000, 2000, 2))
	require.NoError(t, commitSlotGen(t, s, 3, []byte("d"), 6000, 2000, 2))

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(20000, 12)
	s.mu.RUnlock()

	// One discontinuity (gen 0→2, skipping 1).
	assert.Equal(t, 1, strings.Count(playlist, "#EXT-X-DISCONTINUITY\n"))

	// MAP entries: gen 0 and gen 2 (NOT gen 1).
	assert.Contains(t, playlist, `#EXT-X-MAP:URI="init_0.mp4"`)
	assert.NotContains(t, playlist, `#EXT-X-MAP:URI="init_1.mp4"`)
	assert.Contains(t, playlist, `#EXT-X-MAP:URI="init_2.mp4"`)

	// All 4 segments present.
	for i := range 4 {
		assert.Contains(t, playlist, fmt.Sprintf("segment_%d.m4s", i))
	}

	assert.Contains(t, playlist, "#EXT-X-DISCONTINUITY-SEQUENCE:0\n")
}

func TestRenderMediaPlaylist_DiscontinuityOrder(t *testing.T) {
	// Verify the exact ordering: DISCONTINUITY comes before MAP for new generation.
	_, s := setupStreamForPlaylist(t, 2)

	mustAddInit(t, s, 1, []byte("init1"))

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
	_, err := s.AddInitEntry(-1, []byte("init"))
	assert.ErrorIs(t, err, ErrNegativeGeneration)

	// Empty init data should fail.
	_, err = s.AddInitEntry(1, []byte{})
	assert.ErrorIs(t, err, ErrEmptyInitData)

	// Generation 0 already has an init entry (created by Init) → duplicate.
	_, err = s.AddInitEntry(0, []byte("init-dup"))
	assert.ErrorIs(t, err, ErrDuplicateGeneration)

	// Adding generation 1 (> max existing gen 0) should succeed.
	_, err = s.AddInitEntry(1, []byte("init1"))
	assert.NoError(t, err)

	// Adding generation 1 again → duplicate.
	_, err = s.AddInitEntry(1, []byte("init1-dup"))
	assert.ErrorIs(t, err, ErrDuplicateGeneration)

	// Adding generation 3 (skipping 2) should succeed.
	_, err = s.AddInitEntry(3, []byte("init3"))
	assert.NoError(t, err)

	// Adding generation 2 (less than max existing 3) → not monotonic.
	_, err = s.AddInitEntry(2, []byte("init2"))
	assert.ErrorIs(t, err, ErrGenerationNotMonotonic)
}

func TestAddInitEntry_ClonesData(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	mustInit(t, store, "g", meta, []byte("init"), 10, testSegmentBytes, 5)
	s := store.Get("g")

	data := []byte{0x01, 0x02, 0x03}
	mustAddInit(t, s, 1, data)

	// Mutate the caller's slice.
	data[0] = 0xFF

	entry, ok := s.GetInitEntry(1)
	require.True(t, ok)
	assert.Equal(t, byte(0x01), entry.InitData[0], "AddInitEntry should clone input")
}

func TestInitDedup_SuppressesDiscontinuity(t *testing.T) {
	// When a new generation's init is binary-identical to the current, the
	// playlist should NOT contain a #EXT-X-DISCONTINUITY between them.
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	mustInit(t, store, "g", meta, []byte("init-data"), 20, testSegmentBytes, 5)
	s := store.Get("g")

	// Push gen=0 segments.
	mustCommitSlot(t, s, 0, []byte("seg0"), 1000, 2000)
	mustCommitSlot(t, s, 1, []byte("seg1"), 3000, 2000)

	// Add gen=1 with IDENTICAL init data.
	mustAddInit(t, s, 1, []byte("init-data"))

	// Push gen=1 segments (these overwrite stale gen=0 segments at/after insertion).
	commitSlotGen(t, s, 2, []byte("seg2"), 5000, 2000, 1)
	commitSlotGen(t, s, 3, []byte("seg3"), 7000, 2000, 1)

	// All segments should be eligible.
	clk.Set(time.UnixMilli(8000))

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(8000, 10)
	s.mu.RUnlock()

	// No #EXT-X-DISCONTINUITY should appear — inits are identical.
	assert.NotContains(t, playlist, "#EXT-X-DISCONTINUITY\n",
		"identical init should suppress discontinuity")

	// But segments from both generations should be present.
	assert.Contains(t, playlist, "segment_0.m4s")
	assert.Contains(t, playlist, "segment_3.m4s")
}

func TestInitDedup_PreservesDiscontinuityWhenDifferent(t *testing.T) {
	// When a new generation has DIFFERENT init data, discontinuity is preserved.
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	mustInit(t, store, "g", meta, []byte("init-v1"), 20, testSegmentBytes, 5)
	s := store.Get("g")

	mustCommitSlot(t, s, 0, []byte("seg0"), 1000, 2000)
	mustCommitSlot(t, s, 1, []byte("seg1"), 3000, 2000)

	// Add gen=1 with DIFFERENT init data.
	mustAddInit(t, s, 1, []byte("init-v2"))

	commitSlotGen(t, s, 2, []byte("seg2"), 5000, 2000, 1)
	commitSlotGen(t, s, 3, []byte("seg3"), 7000, 2000, 1)

	clk.Set(time.UnixMilli(8000))

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(8000, 10)
	s.mu.RUnlock()

	// #EXT-X-DISCONTINUITY should be present — inits differ.
	assert.Contains(t, playlist, "#EXT-X-DISCONTINUITY\n",
		"different init should produce discontinuity")
}

func TestInitDedup_ThreeGenerationsNoDiscontinuity(t *testing.T) {
	// Three consecutive generations with identical init data must produce
	// NO discontinuities. This exercises transitive equivalence resolution:
	// gen=2 aliases to gen=1, which aliases to gen=0.
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	mustInit(t, store, "g", meta, []byte("init-data"), 30, testSegmentBytes, 5)
	s := store.Get("g")

	// Gen 0 segments.
	mustCommitSlot(t, s, 0, []byte("s0"), 1000, 2000)
	mustCommitSlot(t, s, 1, []byte("s1"), 3000, 2000)

	// Gen 1 — identical init.
	mustAddInit(t, s, 1, []byte("init-data"))
	commitSlotGen(t, s, 2, []byte("s2"), 5000, 2000, 1)
	commitSlotGen(t, s, 3, []byte("s3"), 7000, 2000, 1)

	// Gen 2 — identical init again.
	mustAddInit(t, s, 2, []byte("init-data"))
	commitSlotGen(t, s, 4, []byte("s4"), 9000, 2000, 2)
	commitSlotGen(t, s, 5, []byte("s5"), 11000, 2000, 2)

	clk.Set(time.UnixMilli(12000))

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(12000, 20)
	s.mu.RUnlock()

	assert.NotContains(t, playlist, "#EXT-X-DISCONTINUITY\n",
		"three identical-init generations should produce no discontinuities;\nplaylist:\n%s", playlist)

	// All segments should be present.
	assert.Contains(t, playlist, "segment_0.m4s")
	assert.Contains(t, playlist, "segment_5.m4s")

	// MAP URI should use the canonical (gen=0) init for all segments.
	assert.Contains(t, playlist, `#EXT-X-MAP:URI="init_0.mp4"`)
	assert.NotContains(t, playlist, `#EXT-X-MAP:URI="init_1.mp4"`)
	assert.NotContains(t, playlist, `#EXT-X-MAP:URI="init_2.mp4"`)
}

func TestInitDedup_MapUriStableAfterWindowShift(t *testing.T) {
	// After all gen=0 segments expire from the window, the MAP URI must
	// still reference the canonical init (init_0.mp4), not init_1.mp4.
	// A URI change would cause the player to re-download the (identical)
	// init and reinitialize its decoder — causing stutter.
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	mustInit(t, store, "g", meta, []byte("init-data"), 30, testSegmentBytes, 2)
	s := store.Get("g")

	// Push gen=0 segments.
	mustCommitSlot(t, s, 0, []byte("s0"), 1000, 2000)
	mustCommitSlot(t, s, 1, []byte("s1"), 3000, 2000)

	// Gen 1 — identical init.
	mustAddInit(t, s, 1, []byte("init-data"))
	commitSlotGen(t, s, 2, []byte("s2"), 5000, 2000, 1)
	commitSlotGen(t, s, 3, []byte("s3"), 7000, 2000, 1)
	commitSlotGen(t, s, 4, []byte("s4"), 9000, 2000, 1)
	commitSlotGen(t, s, 5, []byte("s5"), 11000, 2000, 1)

	// Advance clock so all segments are eligible and old ones get evicted.
	clk.Set(time.UnixMilli(12000))

	// Use a small window so gen=0 segments are pushed out.
	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(12000, 3)
	s.mu.RUnlock()

	// The window should contain only gen=1 segments.
	assert.NotContains(t, playlist, "segment_0.m4s")
	assert.Contains(t, playlist, "segment_5.m4s")

	// MAP URI must still be init_0.mp4 (the canonical generation).
	assert.Contains(t, playlist, `#EXT-X-MAP:URI="init_0.mp4"`,
		"MAP URI should use canonical gen even after all gen=0 segments expired;\nplaylist:\n%s", playlist)
	assert.NotContains(t, playlist, `#EXT-X-MAP:URI="init_1.mp4"`,
		"MAP URI should NOT switch to init_1.mp4;\nplaylist:\n%s", playlist)
}

func TestInitDedup_OverwritesOldSegments(t *testing.T) {
	// Critical: even when init is deduped, generation changes must still
	// overwrite (drop) old segments at/after the new segment's position.
	clk := clock.NewMock(time.UnixMilli(0))
	store := NewStore(clk)
	meta := Metadata{Bandwidth: 1, Codecs: "avc1.64001f", Width: 1, Height: 1, FrameRate: 30, TargetDurationSecs: 2}
	mustInit(t, store, "g", meta, []byte("init-data"), 20, testSegmentBytes, 5)
	s := store.Get("g")

	// Push gen=0 segments at indices 0-3.
	mustCommitSlot(t, s, 0, []byte("old0"), 1000, 2000)
	mustCommitSlot(t, s, 1, []byte("old1"), 3000, 2000)
	mustCommitSlot(t, s, 2, []byte("old2"), 5000, 2000)
	mustCommitSlot(t, s, 3, []byte("old3"), 7000, 2000)

	// Add gen=1 with IDENTICAL init data.
	mustAddInit(t, s, 1, []byte("init-data"))

	// Push gen=1 segment at index 2 — must drop old segments at index 2 and 3.
	err := commitSlotGen(t, s, 2, []byte("new2"), 5000, 2000, 1)
	require.NoError(t, err)

	// Verify old segment at index 3 was dropped.
	clk.Set(time.UnixMilli(8000))

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(8000, 10)
	s.mu.RUnlock()

	assert.Contains(t, playlist, "segment_0.m4s", "old gen=0 before insertion should remain")
	assert.Contains(t, playlist, "segment_1.m4s", "old gen=0 before insertion should remain")
	assert.Contains(t, playlist, "segment_2.m4s", "new gen=1 segment should be present")
	assert.NotContains(t, playlist, "segment_3.m4s", "old gen=0 segment at index 3 should be dropped")
}

// --- Renderer notify tests ---

func TestRunPlaylistRenderer_NotifyDuringMinRenderInterval(t *testing.T) {
	// Regression test: when the renderer enters the minRenderInterval throttle
	// path (sleepMs <= 0), a notifyCh signal must wake it and cause a re-render
	// rather than waiting for the timer.
	//
	// Scenario: clock advances between the two clock reads in the render loop
	// (nowMs at render time vs. sleepMs calculation), putting the renderer into
	// the minRenderInterval path. The mock timer target (clock + 50ms) is never
	// reached because the test doesn't advance the clock further. Without
	// notifyCh in the select, the renderer would deadlock.
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
	err := store.Init("1", meta, []byte("init"), 0, 20, testSegmentBytes, 10, 2, 12)
	require.NoError(t, err)
	t.Cleanup(func() { store.Delete("1") })

	s := store.Get("1")
	require.NotNil(t, s)

	// Push a segment with a future timestamp (ts=5000) while clock is at 0.
	buf, ok := s.AcquireSlot()
	require.True(t, ok)
	_, _ = buf.ReadFrom(bytes.NewReader([]byte("seg0")))
	require.NoError(t, s.CommitSlot(0, buf, 5000, 2000, 0))

	// Advance clock past the segment so it becomes eligible. The renderer
	// will wake (timer target was 5000), render segment_0, and then see no
	// future segments → wait on notifyCh.
	clk.Set(time.UnixMilli(6000))

	// Wait for the renderer to produce a playlist with segment_0.
	require.Eventually(t, func() bool {
		p := s.CachedPlaylist()
		return p != "" && strings.Contains(p, "segment_0.m4s")
	}, 2*time.Second, 10*time.Millisecond)

	// Now push a segment at ts=7000 (future, > clock 6000).
	buf, ok = s.AcquireSlot()
	require.True(t, ok)
	_, _ = buf.ReadFrom(bytes.NewReader([]byte("seg1")))
	require.NoError(t, s.CommitSlot(1, buf, 7000, 2000, 0))

	// The renderer wakes from notifyCh, renders (segment_1 not yet eligible
	// at clock 6000), and computes sleepMs = 7000 - 6000 = 1000. Timer target
	// = 6000 + 1000 = 7000.
	//
	// Now advance clock to 8000 (past the timer target) so the renderer wakes,
	// but then also push segment_2 at ts=9000 before the renderer completes
	// its next loop iteration. This sets up the race where the second clock
	// read (sleepMs) sees 8000 while the render used nowMs from the first
	// clock read which might be stale.
	clk.Set(time.UnixMilli(8000))

	// Push segment_2 at ts=9000 to signal notifyCh. The renderer may be in
	// the minRenderInterval path if it hit sleepMs <= 0 due to the clock race.
	buf, ok = s.AcquireSlot()
	require.True(t, ok)
	_, _ = buf.ReadFrom(bytes.NewReader([]byte("seg2")))
	require.NoError(t, s.CommitSlot(2, buf, 9000, 2000, 0))

	// Advance clock past segment_2's timestamp so it's eligible.
	clk.Set(time.UnixMilli(10000))

	// Push segment_3 to wake the renderer one more time.
	buf, ok = s.AcquireSlot()
	require.True(t, ok)
	_, _ = buf.ReadFrom(bytes.NewReader([]byte("seg3")))
	require.NoError(t, s.CommitSlot(3, buf, 11000, 2000, 0))

	// The renderer must eventually produce a playlist containing segment_2.
	// Before the fix (adding notifyCh to minRenderInterval select), this
	// would time out because the renderer could get stuck on a mock timer
	// that never fires.
	require.Eventually(t, func() bool {
		p := s.CachedPlaylist()
		return p != "" && strings.Contains(p, "segment_2.m4s")
	}, 2*time.Second, 10*time.Millisecond)
}

func TestRunPlaylistRenderer_NotifyRacesWithTimer(t *testing.T) {
	// Verify that a notifyCh signal arriving at the same time as a timer fire
	// does not deadlock or lose the notification.
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
	err := store.Init("1", meta, []byte("init"), 0, 20, testSegmentBytes, 10, 2, 12)
	require.NoError(t, err)
	t.Cleanup(func() { store.Delete("1") })

	s := store.Get("1")
	require.NotNil(t, s)

	// Push segment_0 at ts=2000, advance clock to make it eligible.
	buf, ok := s.AcquireSlot()
	require.True(t, ok)
	_, _ = buf.ReadFrom(bytes.NewReader([]byte("seg0")))
	require.NoError(t, s.CommitSlot(0, buf, 2000, 2000, 0))
	clk.Set(time.UnixMilli(3000))

	require.Eventually(t, func() bool {
		p := s.CachedPlaylist()
		return p != "" && strings.Contains(p, "segment_0.m4s")
	}, 2*time.Second, 10*time.Millisecond)

	// Push segment_1 at ts=5000. The renderer will set a timer for 5000.
	buf, ok = s.AcquireSlot()
	require.True(t, ok)
	_, _ = buf.ReadFrom(bytes.NewReader([]byte("seg1")))
	require.NoError(t, s.CommitSlot(1, buf, 5000, 2000, 0))

	// Simultaneously: advance clock past the timer target AND push a new
	// segment. Both the timer fire (from Set) and the notifyCh signal
	// (from CommitSlot) arrive at the renderer's select. This must not
	// deadlock regardless of which one the select picks.
	clk.Set(time.UnixMilli(6000))
	buf, ok = s.AcquireSlot()
	require.True(t, ok)
	_, _ = buf.ReadFrom(bytes.NewReader([]byte("seg2")))
	require.NoError(t, s.CommitSlot(2, buf, 8000, 2000, 0))

	// Advance past segment_2 and push one more to ensure the renderer
	// processes everything.
	clk.Set(time.UnixMilli(9000))
	buf, ok = s.AcquireSlot()
	require.True(t, ok)
	_, _ = buf.ReadFrom(bytes.NewReader([]byte("seg3")))
	require.NoError(t, s.CommitSlot(3, buf, 10000, 2000, 0))

	// All segments through segment_2 must appear.
	require.Eventually(t, func() bool {
		p := s.CachedPlaylist()
		return p != "" && strings.Contains(p, "segment_2.m4s")
	}, 2*time.Second, 10*time.Millisecond)
}

// ---------------------------------------------------------------------------
// Full generation-change lifecycle integration test
// ---------------------------------------------------------------------------

// TestGenerationChangeLifecycle simulates the exact production scenario:
//
//  1. Old pipeline (gen=0) is running and has pushed segments well ahead of now.
//  2. Schedule change: new init is pushed (identical binary), gen=1 segments
//     replace the old future segments.
//  3. The playlist is checked at multiple time points for anomalies:
//     - No #EXT-X-DISCONTINUITY (init is identical)
//     - No #EXT-X-MAP change (MAP URI stays at canonical generation)
//     - Continuous #EXT-X-PROGRAM-DATE-TIME (no gaps or overlaps)
//     - Monotonically increasing segment indices
//
// This test uses epoch-scale timestamps and indices to match production.
func TestGenerationChangeLifecycle(t *testing.T) {
	baseMs := int64(1_775_340_000_000) // ~2026-04-04
	segDur := int64(2000)
	segDurU32 := uint32(2000)
	windowSize := 8

	clk := clock.NewMock(time.UnixMilli(baseMs))
	store := NewStore(clk)
	meta := Metadata{
		Bandwidth:          6_000_000,
		Codecs:             "hvc1.1.6.L120.90,mp4a.40.2",
		Width:              1920,
		Height:             1080,
		FrameRate:          23.976,
		TargetDurationSecs: 2,
	}
	initData := []byte("identical-init-segment-binary-data")
	err := store.Init("s", meta, initData, 0, 50, 4096, 10, 5, windowSize)
	require.NoError(t, err)
	s := store.Get("s")
	require.NotNil(t, s)
	t.Cleanup(func() { store.Delete("s") })

	// --- Phase 1: Old pipeline (gen=0) pushes segments way ahead of now ---
	baseIdx := uint32(baseMs / segDur)
	for i := uint32(0); i < 20; i++ {
		idx := baseIdx + i
		ts := baseMs + int64(i)*segDur
		err := commitSlotGen(t, s, idx, []byte("old-data"), ts, segDurU32, 0)
		require.NoError(t, err, "gen=0 segment %d", idx)
	}

	// --- Verify playlist before generation change ---
	nowMs := baseMs + 10*segDur
	clk.Set(time.UnixMilli(nowMs))

	s.mu.RLock()
	playlistBefore, _ := s.renderMediaPlaylist(nowMs, windowSize)
	s.mu.RUnlock()

	require.NotEmpty(t, playlistBefore, "should have playlist before gen change")
	assert.NotContains(t, playlistBefore, "#EXT-X-DISCONTINUITY\n",
		"no discontinuity before gen change")
	assert.Contains(t, playlistBefore, `#EXT-X-MAP:URI="init_0.mp4"`)

	t.Log("=== Playlist BEFORE generation change ===")
	t.Log(playlistBefore)

	// --- Phase 2: Generation change ---
	mustAddInit(t, s, 1, initData)

	newRefMs := nowMs + 4*segDur // new pipeline ~4 segments ahead
	newBaseIdx := uint32(newRefMs / segDur)

	for i := uint32(0); i < 10; i++ {
		idx := newBaseIdx + i
		ts := newRefMs + int64(i)*segDur
		err := commitSlotGen(t, s, idx, []byte("new-data"), ts, segDurU32, 1)
		require.NoError(t, err, "gen=1 segment %d", idx)
	}

	// --- Phase 3: Check playlist immediately after generation change ---
	s.mu.RLock()
	playlistAfter, _ := s.renderMediaPlaylist(nowMs, windowSize)
	s.mu.RUnlock()

	t.Log("=== Playlist AFTER generation change ===")
	t.Log(playlistAfter)

	assert.NotContains(t, playlistAfter, "#EXT-X-DISCONTINUITY\n",
		"no discontinuity after gen change with identical init")
	assertSingleMapURI(t, playlistAfter, "init_0.mp4")

	// --- Phase 4: Advance time past all old segments ---
	futureMs := newRefMs + 10*segDur
	clk.Set(time.UnixMilli(futureMs))

	s.mu.RLock()
	playlistFuture, _ := s.renderMediaPlaylist(futureMs, windowSize)
	s.mu.RUnlock()

	t.Log("=== Playlist AFTER all old segments expired ===")
	t.Log(playlistFuture)

	assert.NotContains(t, playlistFuture, "#EXT-X-DISCONTINUITY\n",
		"no discontinuity even after old segments expired")
	assertSingleMapURI(t, playlistFuture, "init_0.mp4")

	// --- Phase 4b: Check at the exact transition point ---
	// Advance to where the first gen=1 segment just became eligible.
	transitionMs := newRefMs
	clk.Set(time.UnixMilli(transitionMs))

	s.mu.RLock()
	playlistTransition, _ := s.renderMediaPlaylist(transitionMs, windowSize)
	s.mu.RUnlock()

	t.Log("=== Playlist at TRANSITION point (first gen=1 segment eligible) ===")
	t.Log(playlistTransition)

	assert.NotContains(t, playlistTransition, "#EXT-X-DISCONTINUITY\n",
		"no discontinuity at transition point")
	assertSingleMapURI(t, playlistTransition, "init_0.mp4")

	// --- Phase 5: Verify continuous PDTs in ALL phases ---
	for _, pl := range []struct {
		name     string
		playlist string
	}{
		{"before", playlistBefore},
		{"after", playlistAfter},
		{"transition", playlistTransition},
		{"future", playlistFuture},
	} {
		assertContinuousPDTs(t, pl.playlist, segDur, pl.name)
		assertMonotonicIndices(t, pl.playlist, pl.name)
	}
}

func assertSingleMapURI(t *testing.T, playlist, expectedFile string) {
	t.Helper()
	count := strings.Count(playlist, "#EXT-X-MAP:")
	assert.Equal(t, 1, count,
		"expected exactly 1 #EXT-X-MAP, got %d\nplaylist:\n%s", count, playlist)
	expected := fmt.Sprintf(`#EXT-X-MAP:URI="%s"`, expectedFile)
	assert.Contains(t, playlist, expected,
		"MAP URI should be %s\nplaylist:\n%s", expectedFile, playlist)
}

func assertContinuousPDTs(t *testing.T, playlist string, expectedDurMs int64, label string) {
	t.Helper()
	lines := strings.Split(playlist, "\n")

	var pdts []time.Time
	var durs []float64

	for _, line := range lines {
		if strings.HasPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:") {
			tsStr := strings.TrimPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:")
			ts, err := time.Parse("2006-01-02T15:04:05.000Z", tsStr)
			require.NoError(t, err, "parse PDT %q in %s", tsStr, label)
			pdts = append(pdts, ts)
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			durStr := strings.TrimPrefix(line, "#EXTINF:")
			durStr = strings.TrimSuffix(durStr, ",")
			var dur float64
			_, err := fmt.Sscanf(durStr, "%f", &dur)
			require.NoError(t, err, "parse EXTINF %q in %s", durStr, label)
			durs = append(durs, dur)
		}
	}

	require.Equal(t, len(pdts), len(durs),
		"PDT count (%d) != EXTINF count (%d) in %s", len(pdts), len(durs), label)

	for i := 1; i < len(pdts); i++ {
		prevEnd := pdts[i-1].Add(time.Duration(durs[i-1]*1000) * time.Millisecond)
		gap := pdts[i].Sub(prevEnd)
		assert.InDelta(t, 0, gap.Milliseconds(), 1,
			"PDT gap of %dms between seg %d and %d in %s\nplaylist:\n%s",
			gap.Milliseconds(), i-1, i, label, playlist)
	}
}

func assertMonotonicIndices(t *testing.T, playlist, label string) {
	t.Helper()
	lines := strings.Split(playlist, "\n")

	var indices []uint32
	for _, line := range lines {
		if strings.HasPrefix(line, "segment_") && strings.HasSuffix(line, ".m4s") {
			numStr := strings.TrimPrefix(line, "segment_")
			numStr = strings.TrimSuffix(numStr, ".m4s")
			var idx uint32
			_, err := fmt.Sscanf(numStr, "%d", &idx)
			require.NoError(t, err, "parse segment index %q in %s", numStr, label)
			indices = append(indices, idx)
		}
	}

	for i := 1; i < len(indices); i++ {
		assert.Equal(t, indices[i-1]+1, indices[i],
			"non-consecutive indices: %d then %d in %s\nplaylist:\n%s",
			indices[i-1], indices[i], label, playlist)
	}
}
