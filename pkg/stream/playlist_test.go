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
	err := store.Init("1", meta, []byte("init"), 50, testSegmentBytes, 20, 5, 12)
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
	// Rendered playlists carry a per-viewer-token placeholder (stripped at
	// serve time via ResolveViewerToken). Assert on the placeholdered form.
	assert.Contains(t, playlist, "#EXT-X-MAP:URI=\"init.mp4"+vtPlaceholder+"\"")

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
	// Rendered playlists carry a per-viewer-token placeholder (stripped at
	// serve time via ResolveViewerToken). Assert on the placeholdered form.
	assert.Contains(t, playlist, "#EXT-X-MAP:URI=\"init.mp4"+vtPlaceholder+"\"")
	// No DISCONTINUITY-SEQUENCE header.
	assert.NotContains(t, playlist, "#EXT-X-DISCONTINUITY-SEQUENCE")
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
	err := store.Init("1", meta, []byte("init"), 20, testSegmentBytes, 10, 2, 12)
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
	err := store.Init("1", meta, []byte("init"), 20, testSegmentBytes, 10, 2, 12)
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
