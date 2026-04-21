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
	return setupStreamForPlaylistWithLookahead(t, targetDurationSecs, testMaxLookaheadMs)
}

// setupStreamForPlaylistWithLookahead is like setupStreamForPlaylist but lets
// the test pick the per-stream look-ahead cap. Used by tests that exercise
// the playlist tail running ahead of wall clock.
func setupStreamForPlaylistWithLookahead(t *testing.T, targetDurationSecs int, maxLookaheadMs int64) (*Store, *Stream) {
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
	err := store.Init("1", meta, []byte("init"), 50, testSegmentBytes, 20, 5, 12, maxLookaheadMs)
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
	// No mintToken configured: init URI is emitted without a ?vt= query.
	assert.Contains(t, playlist, "#EXT-X-MAP:URI=\"init.mp4\"")

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
	// Mix of past and future segments relative to nowMs=10000. Uses the
	// zero-lookahead helper, so the cutoff collapses to nowMs.
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
	// Zero-lookahead baseline: nextEligibleMs is the timestamp of the
	// first segment past nowMs. Shifted-cutoff behavior is covered by
	// TestRenderMediaPlaylist_LookaheadExcludesBeyondCap.
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

// --- Look-ahead cap tests ---

// TestRenderMediaPlaylist_LookaheadIncludesFuturePDT asserts that a segment
// with PDT ahead of nowMs but within maxLookaheadMs appears in the playlist.
func TestRenderMediaPlaylist_LookaheadIncludesFuturePDT(t *testing.T) {
	// Target duration 2s, look-ahead 6s (3× multiplier).
	_, s := setupStreamForPlaylistWithLookahead(t, 2, 6000)

	mustCommitSlot(t, s, 0, []byte("d"), 2000, 2000)
	mustCommitSlot(t, s, 1, []byte("d"), 4000, 2000)
	mustCommitSlot(t, s, 2, []byte("d"), 6000, 2000)

	// nowMs=1000, cap=1000+6000=7000. All three segments are within the cap.
	s.mu.RLock()
	playlist, nextMs := s.renderMediaPlaylist(1000, 12)
	s.mu.RUnlock()

	assert.Contains(t, playlist, "segment_0.m4s")
	assert.Contains(t, playlist, "segment_1.m4s")
	assert.Contains(t, playlist, "segment_2.m4s")
	assert.Equal(t, int64(0), nextMs, "no segment past the cap")
}

// TestRenderMediaPlaylist_LookaheadExcludesBeyondCap asserts that a segment
// whose PDT exceeds nowMs + maxLookaheadMs does not appear.
func TestRenderMediaPlaylist_LookaheadExcludesBeyondCap(t *testing.T) {
	_, s := setupStreamForPlaylistWithLookahead(t, 2, 6000)

	mustCommitSlot(t, s, 0, []byte("d"), 2000, 2000)  // within cap
	mustCommitSlot(t, s, 1, []byte("d"), 6000, 2000)  // within cap
	mustCommitSlot(t, s, 2, []byte("d"), 10000, 2000) // beyond cap

	// nowMs=1000, cap=7000. Segment 2 at ts=10000 is past the cap.
	s.mu.RLock()
	playlist, nextMs := s.renderMediaPlaylist(1000, 12)
	s.mu.RUnlock()

	assert.Contains(t, playlist, "segment_0.m4s")
	assert.Contains(t, playlist, "segment_1.m4s")
	assert.NotContains(t, playlist, "segment_2.m4s")
	assert.Equal(t, int64(10000), nextMs, "first segment past cap")
}

// TestRenderMediaPlaylist_LookaheadTailTracksCap asserts the playlist tail
// sits at roughly nowMs + maxLookaheadMs when the transcoder is running far
// ahead — the stutter-repro configuration the feature exists to fix.
func TestRenderMediaPlaylist_LookaheadTailTracksCap(t *testing.T) {
	const durMs = 2000
	_, s := setupStreamForPlaylistWithLookahead(t, 2, 6000)

	// 50 segments covering 100 seconds, all ahead of wall clock.
	for i := range uint32(50) {
		mustCommitSlot(t, s, i, []byte("d"), int64(i)*durMs, durMs)
	}

	// nowMs=0, cap=6000. Eligible segments: indices 0..3 (ts 0, 2000, 4000, 6000).
	s.mu.RLock()
	playlist, nextMs := s.renderMediaPlaylist(0, 12)
	s.mu.RUnlock()

	assert.Contains(t, playlist, "segment_0.m4s")
	assert.Contains(t, playlist, "segment_3.m4s")
	assert.NotContains(t, playlist, "segment_4.m4s")
	// Tail PDT ≈ now + maxLookaheadMs.
	assert.Contains(t, playlist, "#EXT-X-PROGRAM-DATE-TIME:1970-01-01T00:00:06.000Z")
	assert.Equal(t, int64(8000), nextMs, "first segment past the cap is index 4 at ts=8000")
}

// TestRenderMediaPlaylist_LookaheadZeroPinsAtWallClock confirms that a zero
// look-ahead keeps the legacy "tail at wall clock" behavior.
func TestRenderMediaPlaylist_LookaheadZeroPinsAtWallClock(t *testing.T) {
	_, s := setupStreamForPlaylistWithLookahead(t, 2, 0)

	mustCommitSlot(t, s, 0, []byte("d"), 2000, 2000) // eligible at now=4000
	mustCommitSlot(t, s, 1, []byte("d"), 4000, 2000) // eligible (ts == now)
	mustCommitSlot(t, s, 2, []byte("d"), 6000, 2000) // not eligible (future)

	s.mu.RLock()
	playlist, nextMs := s.renderMediaPlaylist(4000, 12)
	s.mu.RUnlock()

	assert.Contains(t, playlist, "segment_0.m4s")
	assert.Contains(t, playlist, "segment_1.m4s")
	assert.NotContains(t, playlist, "segment_2.m4s")
	assert.Equal(t, int64(6000), nextMs)
}

// --- EXT-X-SERVER-CONTROL:HOLD-BACK tests ---

// TestRenderMediaPlaylist_HoldBackMatchesLookahead asserts HOLD-BACK equals
// the configured look-ahead cap in seconds (formatted to 3 decimal places).
func TestRenderMediaPlaylist_HoldBackMatchesLookahead(t *testing.T) {
	// target=2s, lookahead=6s. HOLD-BACK should be 6.000.
	_, s := setupStreamForPlaylistWithLookahead(t, 2, 6000)
	mustCommitSlot(t, s, 0, []byte("d"), 2000, 2000)

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(10000, 12)
	s.mu.RUnlock()

	assert.Contains(t, playlist, "#EXT-X-SERVER-CONTROL:HOLD-BACK=6.000\n")
}

// TestRenderMediaPlaylist_HoldBackClampedToSpecMinimum asserts HOLD-BACK
// clamps up to 3 × target-duration when the configured look-ahead is below
// the spec minimum (draft-pantos-hls-rfc8216bis requires HOLD-BACK >=
// 3 × Target Duration).
func TestRenderMediaPlaylist_HoldBackClampedToSpecMinimum(t *testing.T) {
	// target=4s, lookahead=0 → clamp to 12.000.
	_, s := setupStreamForPlaylistWithLookahead(t, 4, 0)
	mustCommitSlot(t, s, 0, []byte("d"), 2000, 4000)

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(10000, 12)
	s.mu.RUnlock()

	assert.Contains(t, playlist, "#EXT-X-SERVER-CONTROL:HOLD-BACK=12.000\n")
}

// TestRenderMediaPlaylist_HoldBackHeaderOrder asserts HOLD-BACK sits right
// after EXT-X-INDEPENDENT-SEGMENTS and before EXT-X-TARGETDURATION.
func TestRenderMediaPlaylist_HoldBackHeaderOrder(t *testing.T) {
	_, s := setupStreamForPlaylistWithLookahead(t, 2, 6000)
	mustCommitSlot(t, s, 0, []byte("d"), 2000, 2000)

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(10000, 12)
	s.mu.RUnlock()

	indep := strings.Index(playlist, "#EXT-X-INDEPENDENT-SEGMENTS")
	sctrl := strings.Index(playlist, "#EXT-X-SERVER-CONTROL:")
	target := strings.Index(playlist, "#EXT-X-TARGETDURATION:")
	require.Greater(t, indep, -1)
	require.Greater(t, sctrl, -1)
	require.Greater(t, target, -1)
	assert.Less(t, indep, sctrl)
	assert.Less(t, sctrl, target)
}

// --- Contiguity gate tests ---

// TestRenderMediaPlaylist_ContiguityGate_TruncatesAtFirstGap asserts that a
// gap within the window truncates the playlist at the segment before the gap.
// Scenario: indices [0,1,2,4] committed (index 3 still missing). The playlist
// must end at index 2.
func TestRenderMediaPlaylist_ContiguityGate_TruncatesAtFirstGap(t *testing.T) {
	_, s := setupStreamForPlaylistWithLookahead(t, 2, 10000)

	// Commit 0,1,2 in order, then 4. Index 3 is missing.
	mustCommitSlot(t, s, 0, []byte("d"), 2000, 2000)
	mustCommitSlot(t, s, 1, []byte("d"), 4000, 2000)
	mustCommitSlot(t, s, 2, []byte("d"), 6000, 2000)
	mustCommitSlot(t, s, 4, []byte("d"), 10000, 2000)

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(0, 12)
	s.mu.RUnlock()

	assert.Contains(t, playlist, "segment_0.m4s")
	assert.Contains(t, playlist, "segment_1.m4s")
	assert.Contains(t, playlist, "segment_2.m4s")
	assert.NotContains(t, playlist, "segment_3.m4s")
	assert.NotContains(t, playlist, "segment_4.m4s",
		"contiguity gate must truncate before the gap even though index 4 is in the cap")
}

// TestRenderMediaPlaylist_ContiguityGate_ExtendsOnceGapFilled asserts the
// playlist extends through previously-truncated segments as soon as the
// missing index arrives — preserving HLS's append-only invariant.
func TestRenderMediaPlaylist_ContiguityGate_ExtendsOnceGapFilled(t *testing.T) {
	_, s := setupStreamForPlaylistWithLookahead(t, 2, 10000)

	mustCommitSlot(t, s, 0, []byte("d"), 2000, 2000)
	mustCommitSlot(t, s, 1, []byte("d"), 4000, 2000)
	mustCommitSlot(t, s, 2, []byte("d"), 6000, 2000)
	mustCommitSlot(t, s, 4, []byte("d"), 10000, 2000) // leapfrog

	// Before: tail at index 2.
	s.mu.RLock()
	before, _ := s.renderMediaPlaylist(0, 12)
	s.mu.RUnlock()
	assert.NotContains(t, before, "segment_4.m4s")

	// The missing index 3 arrives.
	mustCommitSlot(t, s, 3, []byte("d"), 8000, 2000)

	s.mu.RLock()
	after, _ := s.renderMediaPlaylist(0, 12)
	s.mu.RUnlock()
	assert.Contains(t, after, "segment_3.m4s")
	assert.Contains(t, after, "segment_4.m4s", "playlist extends through 4 once 3 fills the gap")
}

// TestRenderMediaPlaylist_ContiguityGate_GapBeforeWindowIsNoOp asserts that
// a gap entirely before the sliding window (i.e. outside the published
// segments) does not cause truncation. The window itself is contiguous.
func TestRenderMediaPlaylist_ContiguityGate_GapBeforeWindowIsNoOp(t *testing.T) {
	// windowSize=3; commit indices [0,1,3,4,5]. With windowSize=3 applied to
	// eligible set [0,1,3,4,5] the window becomes [3,4,5] — contiguous.
	_, s := setupStreamForPlaylistWithLookahead(t, 2, 20000)

	mustCommitSlot(t, s, 0, []byte("d"), 2000, 2000)
	mustCommitSlot(t, s, 1, []byte("d"), 4000, 2000)
	// index 2 missing
	mustCommitSlot(t, s, 3, []byte("d"), 8000, 2000)
	mustCommitSlot(t, s, 4, []byte("d"), 10000, 2000)
	mustCommitSlot(t, s, 5, []byte("d"), 12000, 2000)

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(0, 3)
	s.mu.RUnlock()

	assert.Contains(t, playlist, "segment_3.m4s")
	assert.Contains(t, playlist, "segment_4.m4s")
	assert.Contains(t, playlist, "segment_5.m4s")
	// Segments 0,1 are outside the window; segment 2 is the pre-window gap.
	assert.NotContains(t, playlist, "segment_0.m4s")
	assert.NotContains(t, playlist, "segment_1.m4s")
	assert.NotContains(t, playlist, "segment_2.m4s")
}

// TestRenderMediaPlaylist_ContiguityGate_OutOfOrderCommit asserts that
// receiving segments out of index order (index 4 committed before 3) still
// ends up with a correct, contiguous playlist after the missing index fills.
func TestRenderMediaPlaylist_ContiguityGate_OutOfOrderCommit(t *testing.T) {
	_, s := setupStreamForPlaylistWithLookahead(t, 2, 20000)

	mustCommitSlot(t, s, 0, []byte("d"), 2000, 2000)
	mustCommitSlot(t, s, 1, []byte("d"), 4000, 2000)
	mustCommitSlot(t, s, 2, []byte("d"), 6000, 2000)
	// Transcoder delivered 4 before 3.
	mustCommitSlot(t, s, 4, []byte("d"), 10000, 2000)

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(0, 12)
	s.mu.RUnlock()
	assert.NotContains(t, playlist, "segment_4.m4s")

	mustCommitSlot(t, s, 3, []byte("d"), 8000, 2000)

	s.mu.RLock()
	playlist, _ = s.renderMediaPlaylist(0, 12)
	s.mu.RUnlock()
	assert.Contains(t, playlist, "segment_0.m4s")
	assert.Contains(t, playlist, "segment_1.m4s")
	assert.Contains(t, playlist, "segment_2.m4s")
	assert.Contains(t, playlist, "segment_3.m4s")
	assert.Contains(t, playlist, "segment_4.m4s")
}

func TestRenderMediaPlaylist_SingleGeneration_NoDiscontinuity(t *testing.T) {
	_, s := setupStreamForPlaylist(t, 2)

	for i := range uint32(3) {
		mustCommitSlot(t, s, i, []byte("d"), int64(i)*2000, 2000)
	}

	s.mu.RLock()
	playlist, _ := s.renderMediaPlaylist(20000, 12)
	s.mu.RUnlock()

	// No discontinuity tags.
	assert.NotContains(t, playlist, "#EXT-X-DISCONTINUITY")
	// Should have exactly one EXT-X-MAP.
	assert.Equal(t, 1, strings.Count(playlist, "#EXT-X-MAP:URI="))
	// No mintToken configured: init URI is emitted without a ?vt= query.
	assert.Contains(t, playlist, "#EXT-X-MAP:URI=\"init.mp4\"")
	// No DISCONTINUITY-SEQUENCE header.
	assert.NotContains(t, playlist, "#EXT-X-DISCONTINUITY-SEQUENCE")
}

// TestRunPlaylistRenderer_WakesAtCapCrossing asserts the renderer wakes when
// the next pending segment crosses the look-ahead cap (ts - maxLookaheadMs),
// not at the segment's raw timestamp. Without this, a batch of future
// segments pushed once with no follow-up commits appears in the playlist
// up to maxLookaheadMs late, because notifyCh — which otherwise masks a
// too-long timer — doesn't fire.
func TestRunPlaylistRenderer_WakesAtCapCrossing(t *testing.T) {
	const lookaheadMs = 6000
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
	require.NoError(t, store.Init("1", meta, []byte("init"), 20, testSegmentBytes, 10, 2, 12, lookaheadMs))
	t.Cleanup(func() { store.Delete("1") })

	s := store.Get("1")
	require.NotNil(t, s)

	// Push two segments: one within the cap at clock=0 (ts=4000, cap=6000),
	// one beyond (ts=12000, cap-crossing at clock=6000).
	buf, ok := s.AcquireSlot()
	require.True(t, ok)
	_, _ = buf.ReadFrom(bytes.NewReader([]byte("seg0")))
	require.NoError(t, s.CommitSlot(0, buf, 4000, 2000, 0))

	buf, ok = s.AcquireSlot()
	require.True(t, ok)
	_, _ = buf.ReadFrom(bytes.NewReader([]byte("seg1")))
	require.NoError(t, s.CommitSlot(1, buf, 12000, 2000, 0))

	// Wait for the first render (segment_0 visible, segment_1 past cap).
	require.Eventually(t, func() bool {
		p := s.CachedPlaylist()
		return p != "" && strings.Contains(p, "segment_0.m4s") && !strings.Contains(p, "segment_1.m4s")
	}, 2*time.Second, 10*time.Millisecond)

	// Advance clock to exactly the cap-crossing time (12000 - 6000 = 6000).
	// The renderer's timer should have been set for this moment, not for
	// clock=12000. If it fires, segment_1 appears without any new commit
	// to kick notifyCh.
	clk.Set(time.UnixMilli(6000))

	require.Eventually(t, func() bool {
		return strings.Contains(s.CachedPlaylist(), "segment_1.m4s")
	}, 2*time.Second, 10*time.Millisecond)
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
	err := store.Init("1", meta, []byte("init"), 20, testSegmentBytes, 10, 2, 12, testMaxLookaheadMs)
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
	err := store.Init("1", meta, []byte("init"), 20, testSegmentBytes, 10, 2, 12, testMaxLookaheadMs)
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
