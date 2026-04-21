package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/dskiff/streamloom/pkg/config"
	"github.com/dskiff/streamloom/pkg/stream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- GET /stream/{streamID}/segment_{index}.m4s tests ---

func TestGetSegment_Success(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	router, store, _ := testStreamRouter(t, clk)
	initStream(t, store, "1")

	s := store.Get("1")
	require.NotNil(t, s)

	segmentData := []byte("segment-5-data")
	commitSegment(t, s, 5, segmentData, 5000)

	req := httptest.NewRequest(http.MethodGet, "/stream/1/segment_5.m4s", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, config.MP4_MIME_TYPE, rec.Header().Get("Content-Type"))
	assert.Equal(t, strconv.Itoa(len(segmentData)), rec.Header().Get("Content-Length"))
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	assert.Equal(t, segmentData, rec.Body.Bytes())
}

func TestGetSegment_UnconfiguredStream_Returns503(t *testing.T) {
	router, _, _ := testStreamRouter(t, clock.Real{})

	// Unconfigured streams with valid IDs return 503 (same as configured-but-
	// uninitialized) to prevent stream ID enumeration.
	req := httptest.NewRequest(http.MethodGet, "/stream/999/segment_0.m4s", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))
}

func TestGetSegment_ConfiguredButUninitialized_Returns503(t *testing.T) {
	router, _, _ := testStreamRouter(t, clock.Real{})

	// Stream 1 is configured (has a token) but not initialized.
	req := httptest.NewRequest(http.MethodGet, "/stream/1/segment_0.m4s", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))
}

func TestGetSegment_SegmentNotFound(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	router, store, _ := testStreamRouter(t, clk)
	initStream(t, store, "1")

	req := httptest.NewRequest(http.MethodGet, "/stream/1/segment_99.m4s", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotEqual(t, config.MP4_MIME_TYPE, rec.Header().Get("Content-Type"))
}

func TestGetSegment_InvalidSegmentID(t *testing.T) {
	router, store, _ := testStreamRouter(t, clock.Real{})
	initStream(t, store, "1")

	req := httptest.NewRequest(http.MethodGet, "/stream/1/segment_abc.m4s", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestGetSegment_OverflowSegmentID(t *testing.T) {
	router, store, _ := testStreamRouter(t, clock.Real{})
	initStream(t, store, "1")

	req := httptest.NewRequest(http.MethodGet, "/stream/1/segment_99999999999.m4s", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- GET /stream/{streamID}/init.mp4 tests ---

func TestGetInitMP4_Success(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	router, store, _ := testStreamRouter(t, clk)
	initStream(t, store, "1")

	req := httptest.NewRequest(http.MethodGet, "/stream/1/init.mp4", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, config.MP4_MIME_TYPE, rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	assert.Equal(t, strconv.Itoa(len("init-data")), rec.Header().Get("Content-Length"))
	assert.Equal(t, []byte("init-data"), rec.Body.Bytes())
}

func TestGetInitMP4_UnconfiguredStream_Returns503(t *testing.T) {
	router, _, _ := testStreamRouter(t, clock.Real{})

	// Unconfigured streams with valid IDs return 503 to prevent enumeration.
	req := httptest.NewRequest(http.MethodGet, "/stream/999/init.mp4", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))
}

func TestGetInitMP4_ConfiguredButUninitialized_Returns503(t *testing.T) {
	router, _, _ := testStreamRouter(t, clock.Real{})

	// Stream 1 is configured but not initialized.
	req := httptest.NewRequest(http.MethodGet, "/stream/1/init.mp4", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))
}

// --- GET /stream/{streamID}/media.m3u8 tests ---

func TestMediaPlaylist_UnconfiguredStream_Returns503(t *testing.T) {
	router, _, _ := testStreamRouter(t, clock.Real{})

	// Unconfigured streams with valid IDs return 503 to prevent enumeration.
	req := httptest.NewRequest(http.MethodGet, "/stream/999/media.m3u8", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))
}

func TestMediaPlaylist_ConfiguredButUninitialized_Returns503(t *testing.T) {
	router, _, _ := testStreamRouter(t, clock.Real{})

	// Stream 1 is configured but not initialized.
	req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))
}

func TestMediaPlaylist_WithSegments(t *testing.T) {
	// Start at time 0 so segment commits are accepted (timestamps are in the future).
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _ := testStreamRouter(t, clk)
	initStream(t, store, "1")

	s := store.Get("1")
	require.NotNil(t, s)

	commitSegment(t, s, 0, []byte("seg0"), 2000)
	commitSegment(t, s, 1, []byte("seg1"), 4000)
	commitSegment(t, s, 2, []byte("seg2"), 6000)

	// Advance time so all segments are eligible.
	clk.Set(time.UnixMilli(10000))

	// Wait for renderer to populate the cached playlist.
	require.Eventually(t, func() bool {
		return s.CachedPlaylist() != ""
	}, 2*time.Second, 10*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, config.M3U8_MIME_TYPE, rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	assert.NotEmpty(t, rec.Header().Get("Content-Length"))

	body := rec.Body.String()
	assert.Contains(t, body, "#EXTM3U")
	assert.Contains(t, body, "#EXT-X-VERSION:7")
	assert.Contains(t, body, "#EXT-X-TARGETDURATION:2")
	assert.Contains(t, body, "#EXT-X-MEDIA-SEQUENCE:0")
	assert.Contains(t, body, "#EXT-X-MAP:URI=\"init.mp4\"")
	assert.Contains(t, body, "segment_0.m4s")
	assert.Contains(t, body, "segment_1.m4s")
	assert.Contains(t, body, "segment_2.m4s")
	assert.Contains(t, body, "#EXTINF:2.000,")
	assert.Contains(t, body, "#EXT-X-PROGRAM-DATE-TIME:")
}

func TestMediaPlaylist_WallClockFiltering(t *testing.T) {
	// initStream configures the default look-ahead (3 × target-duration =
	// 6000ms at target=2s). Only segments with ts > now+6000 are filtered.
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _ := testStreamRouter(t, clk)
	initStream(t, store, "1")

	s := store.Get("1")
	require.NotNil(t, s)

	commitSegment(t, s, 0, []byte("seg0"), 1000)
	commitSegment(t, s, 1, []byte("seg1"), 3000)
	commitSegment(t, s, 2, []byte("seg2"), 5000)
	// Beyond now+lookahead = 5000+6000 = 11000; must be excluded.
	commitSegment(t, s, 3, []byte("seg3"), 15000)

	clk.Set(time.UnixMilli(5000))

	require.Eventually(t, func() bool {
		return s.CachedPlaylist() != ""
	}, 2*time.Second, 10*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	assert.Contains(t, body, "segment_0.m4s")
	assert.Contains(t, body, "segment_1.m4s")
	assert.Contains(t, body, "segment_2.m4s")
	// Segment 3's timestamp is past now+lookahead and must not appear.
	assert.NotContains(t, body, "segment_3.m4s")
}

func TestMediaPlaylist_Returns503WhenStreamDeletedWhileWaiting(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _ := testStreamRouter(t, clk)
	initStream(t, store, "1")

	s := store.Get("1")
	require.NotNil(t, s)

	// Do not commit any segments — the handler will block on HasSegments.
	// Delete the stream in a goroutine after a short delay.
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(50 * time.Millisecond)
		store.Delete("1")
	}()

	req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	<-done

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))
}

func TestMediaPlaylist_Returns503WhenPlaylistBecomesEmpty(t *testing.T) {
	// Simulate the edge case where HasPlaylist was closed (playlist was once
	// valid) but the cached playlist has since become "". This can happen
	// when the mock clock moves backward so all segments fall past the
	// look-ahead cap. initStream uses the default 6000ms look-ahead at
	// target=2s; segments must be > now+6000 in the future.
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _ := testStreamRouter(t, clk)
	initStream(t, store, "1")

	s := store.Get("1")
	require.NotNil(t, s)

	// Commit segments in the future and advance clock so they become eligible.
	commitSegment(t, s, 0, []byte("seg0"), 12000)
	commitSegment(t, s, 1, []byte("seg1"), 14000)
	clk.Set(time.UnixMilli(10000))

	require.Eventually(t, func() bool {
		return s.CachedPlaylist() != ""
	}, 2*time.Second, 10*time.Millisecond)

	// Move the clock backward so every segment is past the look-ahead cap
	// (cap = 0 + 6000 = 6000, all segments ts >= 12000).
	clk.Set(time.UnixMilli(0))

	// Poke the renderer to re-render by committing another far-future segment.
	commitSegment(t, s, 2, []byte("seg2"), 16000)

	require.Eventually(t, func() bool {
		return s.CachedPlaylist() == ""
	}, 2*time.Second, 10*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))
}

// initStreamWithLookahead is like initStream but lets the test pick the
// look-ahead cap — needed by the dynamic-TIME-OFFSET tests, which require
// HoldBack > MinHoldBack so the offset can shrink below the spec-minimum
// clamp and actually exercise staleness compensation.
func initStreamWithLookahead(t *testing.T, store *stream.Store, id string, targetSecs int, lookaheadMs int64) {
	t.Helper()
	meta := stream.Metadata{
		Bandwidth:          4000000,
		Codecs:             "avc1.64001f",
		Width:              1920,
		Height:             1080,
		FrameRate:          23.976,
		TargetDurationSecs: targetSecs,
	}
	err := store.Init(id, meta, []byte("init-data"), 10, 1024, 5, 2,
		config.DefaultMediaWindowSize, lookaheadMs)
	require.NoError(t, err)
	t.Cleanup(func() { store.Delete(id) })
}

// --- Dynamic TIME-OFFSET tests ---

// TestMediaPlaylist_StartOffset_FreshMatchesHoldBack asserts that when the
// request arrives at the last segment's end PDT (no staleness), the handler
// emits the unchanged -HoldBack offset — preserving today's behavior on a
// hot cache.
func TestMediaPlaylist_StartOffset_FreshMatchesHoldBack(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _ := testStreamRouter(t, clk)
	initStreamWithLookahead(t, store, "1", 2, 10000) // HoldBack=10, MinHoldBack=6

	s := store.Get("1")
	require.NotNil(t, s)

	commitSegment(t, s, 0, []byte("seg0"), 2000) // ends at 4000
	clk.Set(time.UnixMilli(4000))                // request is fresh: now == endMs

	require.Eventually(t, func() bool {
		return s.CachedPlaylist() != ""
	}, 2*time.Second, 10*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "#EXT-X-START:TIME-OFFSET=-10.000,PRECISE=YES\n")
}

// TestMediaPlaylist_StartOffset_StaleShrinks asserts that when the request
// arrives after EndMs (no new commit between renders), the offset shrinks
// by the staleness. Two viewers fetching the same cached body at staggered
// times then compute the same absolute start content time — cancelling the
// within-segment drift.
func TestMediaPlaylist_StartOffset_StaleShrinks(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _ := testStreamRouter(t, clk)
	initStreamWithLookahead(t, store, "1", 2, 10000) // HoldBack=10, MinHoldBack=6

	s := store.Get("1")
	require.NotNil(t, s)

	commitSegment(t, s, 0, []byte("seg0"), 2000) // ends at 4000

	// Fresh render at clock=4000.
	clk.Set(time.UnixMilli(4000))
	require.Eventually(t, func() bool {
		return s.CachedPlaylist() != ""
	}, 2*time.Second, 10*time.Millisecond)

	// Viewer arrives 1.5s later, no new commit.
	clk.Set(time.UnixMilli(5500))

	req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	// 1.5s stale → offset = 10.0 - 1.5 = 8.5.
	assert.Contains(t, rec.Body.String(), "#EXT-X-START:TIME-OFFSET=-8.500,PRECISE=YES\n")
}

// TestMediaPlaylist_StartOffset_ClampedToMinHoldBack asserts that very stale
// responses never advertise a tighter latency than the HOLD-BACK header
// promises. Otherwise clients that obey TIME-OFFSET but not HOLD-BACK would
// try to play closer to live than the server intends and stutter.
func TestMediaPlaylist_StartOffset_ClampedToMinHoldBack(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _ := testStreamRouter(t, clk)
	initStreamWithLookahead(t, store, "1", 2, 10000) // HoldBack=10, MinHoldBack=6

	s := store.Get("1")
	require.NotNil(t, s)

	commitSegment(t, s, 0, []byte("seg0"), 2000) // ends at 4000

	clk.Set(time.UnixMilli(4000))
	require.Eventually(t, func() bool {
		return s.CachedPlaylist() != ""
	}, 2*time.Second, 10*time.Millisecond)

	// 100s past endMs — formula gives offset = 10-100 = -90 (i.e. positive
	// start position past end); must clamp to MinHoldBack = 6.
	clk.Set(time.UnixMilli(104_000))

	req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "#EXT-X-START:TIME-OFFSET=-6.000,PRECISE=YES\n")
}

// TestMediaPlaylist_StartOffset_TwoStaggeredRequestsAgreeOnStartTime is the
// cross-check: given the same underlying playlist body, two requests at
// different clocks must resolve to the same absolute start content time
// (endMs − offsetSecs × 1000). That's the invariant this whole feature
// exists to preserve.
func TestMediaPlaylist_StartOffset_TwoStaggeredRequestsAgreeOnStartTime(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _ := testStreamRouter(t, clk)
	initStreamWithLookahead(t, store, "1", 2, 10000)

	s := store.Get("1")
	require.NotNil(t, s)

	commitSegment(t, s, 0, []byte("seg0"), 2000) // endMs = 4000

	clk.Set(time.UnixMilli(4000))
	require.Eventually(t, func() bool {
		return s.CachedPlaylist() != ""
	}, 2*time.Second, 10*time.Millisecond)

	// Viewer A at clock = endMs + 200ms.
	clk.Set(time.UnixMilli(4200))
	reqA := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	recA := httptest.NewRecorder()
	router.ServeHTTP(recA, reqA)
	offA := extractStartOffsetSecs(t, recA.Body.String())
	startA := 4000 - int64(offA*1000) // endMs − offset → absolute start ms
	wallA := int64(4200)              // A's wall clock at the moment of request

	// Viewer B at clock = endMs + 1900ms (same cached body, no new commit).
	clk.Set(time.UnixMilli(5900))
	reqB := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	recB := httptest.NewRecorder()
	router.ServeHTTP(recB, reqB)
	offB := extractStartOffsetSecs(t, recB.Body.String())
	startB := 4000 - int64(offB*1000)
	wallB := int64(5900)

	// Both viewers' (wall − start) should match: they would arrive at the
	// same content time after the same elapsed playback. Allow 1ms slack
	// for integer-ms float rounding.
	elapsedA := wallA - startA
	elapsedB := wallB - startB
	assert.InDelta(t, elapsedA, elapsedB, 1,
		"staggered viewers must agree on (wall−start); A=%d B=%d", elapsedA, elapsedB)
}

// TestMediaPlaylist_StartOffset_ContentLengthMatchesBody guards against
// the three-part write drifting out of sync with the Content-Length
// header. The handler composes total = len(Prefix) + len(StartLine) +
// len(Suffix) where StartLine is a freshly-formatted string whose width
// depends on the offset value; a future change to either the header or
// the writes must keep the two in lockstep or clients see a truncated /
// padded body.
func TestMediaPlaylist_StartOffset_ContentLengthMatchesBody(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _ := testStreamRouter(t, clk)
	initStreamWithLookahead(t, store, "1", 2, 10000)

	s := store.Get("1")
	require.NotNil(t, s)

	commitSegment(t, s, 0, []byte("seg0"), 2000) // endMs = 4000

	clk.Set(time.UnixMilli(4000))
	require.Eventually(t, func() bool {
		return s.CachedPlaylist() != ""
	}, 2*time.Second, 10*time.Millisecond)

	// Exercise a few request clocks that produce different StartLine
	// widths (fresh vs stale vs clamped), to make sure the invariant
	// holds across the range of offset magnitudes the handler emits.
	for _, wall := range []int64{4000, 4200, 5500, 104_000} {
		clk.Set(time.UnixMilli(wall))
		req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "wall=%d", wall)
		cl, err := strconv.Atoi(rec.Header().Get("Content-Length"))
		require.NoError(t, err, "wall=%d: Content-Length must parse", wall)
		assert.Equal(t, cl, rec.Body.Len(),
			"wall=%d: Content-Length header (%d) must match body length (%d)",
			wall, cl, rec.Body.Len())
	}
}

// extractStartOffsetSecs parses the TIME-OFFSET value (a positive magnitude)
// out of the single EXT-X-START line in a playlist body.
func extractStartOffsetSecs(t *testing.T, body string) float64 {
	t.Helper()
	const prefix = "#EXT-X-START:TIME-OFFSET=-"
	i := strings.Index(body, prefix)
	require.GreaterOrEqual(t, i, 0, "playlist missing EXT-X-START")
	tail := body[i+len(prefix):]
	j := strings.IndexByte(tail, ',')
	require.GreaterOrEqual(t, j, 0, "EXT-X-START missing comma")
	secs, err := strconv.ParseFloat(tail[:j], 64)
	require.NoError(t, err)
	return secs
}

// --- GET /stream/{streamID}/stream.m3u8 tests ---

func TestStreamM3U8_UnconfiguredStream_Returns503(t *testing.T) {
	router, _, _ := testStreamRouter(t, clock.Real{})

	// Unconfigured streams with valid IDs return 503 to prevent enumeration.
	req := httptest.NewRequest(http.MethodGet, "/stream/999/stream.m3u8", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))
}

func TestStreamM3U8_ConfiguredButUninitialized_Returns503(t *testing.T) {
	router, _, _ := testStreamRouter(t, clock.Real{})

	// Stream 1 is configured but not initialized.
	req := httptest.NewRequest(http.MethodGet, "/stream/1/stream.m3u8", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))
}

// --- Middleware tests ---

func TestNosniffHeader(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	router, store, _ := testStreamRouter(t, clk)
	initStream(t, store, "1")

	s := store.Get("1")
	require.NotNil(t, s)

	// Verify on segment response.
	segmentData := []byte("nosniff-test")
	commitSegment(t, s, 0, segmentData, 5000)

	req := httptest.NewRequest(http.MethodGet, "/stream/1/segment_0.m4s", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))

	// Verify on init response.
	initURL := "/stream/1/init.mp4"
	req = httptest.NewRequest(http.MethodGet, initURL, nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))

	// Verify on master playlist.
	req = httptest.NewRequest(http.MethodGet, "/stream/1/stream.m3u8", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
}

// --- Health check tests ---

func TestStreamServer_Healthz(t *testing.T) {
	router, _, _ := testStreamRouter(t, clock.Real{})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

// --- Context cancellation tests ---

func TestMediaPlaylist_Returns503OnContextCancellation(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _ := testStreamRouter(t, clk)
	initStream(t, store, "1")

	// Do not commit any segments so the handler blocks on HasPlaylist.
	// Create a request with a context we can cancel.
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		router.ServeHTTP(rec, req)
	}()

	// Cancel the context to simulate a timeout.
	cancel()
	<-done

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))
}

// --- Routing tests ---

func TestPublicRoute_InvalidStreamID(t *testing.T) {
	router, _, _ := testStreamRouter(t, clock.Real{})

	// Stream ID with non-alphanumeric characters should return 404 Not Found.
	req := httptest.NewRequest(http.MethodGet, "/stream/a.b/media.m3u8", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}
