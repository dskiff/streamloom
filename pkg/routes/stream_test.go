package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
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

	// Wait for the LAST committed segment to appear in the cache.
	// CommitSlot's notifyCh is a coalescing single-slot channel: if the
	// renderer is mid-render when a follow-up commit fires, it may store
	// a snapshot containing only the earliest segment. Polling just for
	// `!= ""` returns true against that intermediate render, and the
	// follow-up HTTP request races the next re-render.
	require.Eventually(t, func() bool {
		return strings.Contains(s.CachedPlaylist(), "segment_2.m4s")
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

	// Wait for the last eligible segment (seg2) to land in the cache —
	// polling just `!= ""` would race the coalesced notifyCh and
	// short-circuit on a render that only contains seg0.
	require.Eventually(t, func() bool {
		return strings.Contains(s.CachedPlaylist(), "segment_2.m4s")
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
// look-ahead cap — needed by the dynamic-TIME-OFFSET tests so they can
// put the tail far enough ahead of wall clock to exercise the offset
// formula without tripping the MinHoldBack floor.
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
	err := store.Init(id, meta, []byte("init-data"), 20, 1024, 12, 2,
		config.DefaultMediaWindowSize, lookaheadMs)
	require.NoError(t, err)
	t.Cleanup(func() { store.Delete(id) })
}

// --- Sticky TIME-OFFSET tests ---
//
// The master playlist bakes a fresh `?to=<magnitude>` (the tail-to-now
// offset) into the media URI. The player parses that URL once from
// the master and reuses it on every reload. The media handler echoes
// `?to=` verbatim into EXT-X-START, so the rendered tag is
// byte-identical across reloads — current iOS/macOS clients stall
// when the value mutates between reloads. A request without `?to=`
// (or with a malformed value) falls back to rendering WITHOUT
// EXT-X-START; HOLD-BACK still positions the player at the live edge.

// TestMediaPlaylist_Sticky_RendersClientSuppliedOffset asserts the
// happy path on the sticky URL: a request with `?to=` renders the
// playlist with EXT-X-START:TIME-OFFSET equal to the magnitude
// supplied — verbatim, no recomputation against wall clock. Two
// fetches of the same URL at different walls produce byte-identical
// EXT-X-START lines.
func TestMediaPlaylist_Sticky_RendersClientSuppliedOffset(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _ := testStreamRouter(t, clk)
	initStreamWithLookahead(t, store, "1", 2, 10000) // MinHoldBack=6

	s := store.Get("1")
	require.NotNil(t, s)
	commitSegment(t, s, 0, []byte("seg0"), 8000) // endMs = 10000
	require.Eventually(t, func() bool {
		return s.CachedPlaylist() != ""
	}, 2*time.Second, 10*time.Millisecond)

	// Fetch at clock=1500 with a baked offset of 8.500. Server must
	// echo it verbatim, not compute against (endMs - 1500)/1000.
	clk.Set(time.UnixMilli(1500))
	req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8?to=8.500", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	first := rec.Body.String()
	assert.Contains(t, first, "#EXT-X-START:TIME-OFFSET=-8.500,PRECISE=YES\n")

	// Fetch again with the same URL but a wildly different wall clock.
	// Sticky semantics: EXT-X-START must be byte-identical to the first
	// fetch even though wall clock has moved.
	clk.Set(time.UnixMilli(5500))
	req = httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8?to=8.500", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	second := rec.Body.String()
	assert.Contains(t, second, "#EXT-X-START:TIME-OFFSET=-8.500,PRECISE=YES\n",
		"sticky URL must echo the same offset across reloads at any wall clock")
}

// TestMediaPlaylist_Sticky_RenderedOffsetIsFlooredAtMinHoldBack asserts
// that a sticky URL surviving past the floor still emits a spec-compliant
// EXT-X-START. A `?to=` magnitude below MinHoldBackSecs is clamped up
// at render time so an iOS player can keep reusing its captured URL
// indefinitely without ever seeing a sub-floor value.
func TestMediaPlaylist_Sticky_RenderedOffsetIsFlooredAtMinHoldBack(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _ := testStreamRouter(t, clk)
	initStreamWithLookahead(t, store, "1", 2, 10000) // MinHoldBack=6

	s := store.Get("1")
	require.NotNil(t, s)
	commitSegment(t, s, 0, []byte("seg0"), 8000)
	require.Eventually(t, func() bool {
		return s.CachedPlaylist() != ""
	}, 2*time.Second, 10*time.Millisecond)

	// Supply 0.500 — well below MinHoldBack=6.
	req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8?to=0.500", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "#EXT-X-START:TIME-OFFSET=-6.000,PRECISE=YES\n")
}

// TestMediaPlaylist_NoTo_OmitsStartTag asserts the fallback: a media
// fetch without `?to=` renders the playlist body WITHOUT an
// EXT-X-START tag. Clients fall back to HOLD-BACK alone — cross-device
// sync precision is lost on this path but iOS doesn't stall (no tag
// = nothing to mutate). The common flow comes through stream.m3u8
// which bakes `?to=` for sync.
func TestMediaPlaylist_NoTo_OmitsStartTag(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _ := testStreamRouter(t, clk)
	initStreamWithLookahead(t, store, "1", 2, 10000)

	s := store.Get("1")
	require.NotNil(t, s)
	commitSegment(t, s, 0, []byte("seg0"), 8000)
	require.Eventually(t, func() bool {
		return s.CachedPlaylist() != ""
	}, 2*time.Second, 10*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "#EXT-X-START:",
		"media.m3u8 without ?to= must omit EXT-X-START to avoid mutating it across reloads")
	// HOLD-BACK is still there — clients position by that alone.
	assert.Contains(t, body, "#EXT-X-SERVER-CONTROL:HOLD-BACK=")
}

// TestMediaPlaylist_MalformedTo_OmitsStartTag asserts the same
// fallback applies to a malformed/out-of-range `?to=`. A benign client
// mistake or stale URL falls through to the no-tag path rather than
// hitting a new 4xx surface.
func TestMediaPlaylist_MalformedTo_OmitsStartTag(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _ := testStreamRouter(t, clk)
	initStreamWithLookahead(t, store, "1", 2, 10000)

	s := store.Get("1")
	require.NotNil(t, s)
	commitSegment(t, s, 0, []byte("seg0"), 8000)
	require.Eventually(t, func() bool {
		return s.CachedPlaylist() != ""
	}, 2*time.Second, 10*time.Millisecond)

	for _, badTo := range []string{
		"not-a-number",
		"-1.000",      // negative magnitude
		"NaN",         // NaN
		"+Inf",        // Inf
		"99999999999", // beyond maxStickyOffsetSecs ceiling
	} {
		t.Run(badTo, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8?to="+url.QueryEscape(badTo), nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code,
				"malformed ?to= must render normally without EXT-X-START, got %d", rec.Code)
			assert.NotContains(t, rec.Body.String(), "#EXT-X-START:",
				"malformed ?to= must omit EXT-X-START")
		})
	}
}

// TestMediaPlaylist_Sticky_ContentLengthMatchesBody guards against the
// three-part write drifting out of sync with the Content-Length header
// across the range of magnitudes the sticky URL can carry, including
// the no-tag fallback.
func TestMediaPlaylist_Sticky_ContentLengthMatchesBody(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _ := testStreamRouter(t, clk)
	initStreamWithLookahead(t, store, "1", 2, 10000)

	s := store.Get("1")
	require.NotNil(t, s)
	commitSegment(t, s, 0, []byte("seg0"), 8000)
	require.Eventually(t, func() bool {
		return s.CachedPlaylist() != ""
	}, 2*time.Second, 10*time.Millisecond)

	// Different paths through the handler produce different total
	// widths; Content-Length must follow the rendered body in each.
	for _, path := range []string{
		"/stream/1/media.m3u8?to=6.000",
		"/stream/1/media.m3u8?to=8.500",
		"/stream/1/media.m3u8?to=1234.567",
		"/stream/1/media.m3u8", // no-tag fallback
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "path=%s", path)
		cl, err := strconv.Atoi(rec.Header().Get("Content-Length"))
		require.NoError(t, err, "path=%s: Content-Length must parse", path)
		assert.Equal(t, cl, rec.Body.Len(),
			"path=%s: Content-Length header (%d) must match body length (%d)",
			path, cl, rec.Body.Len())
	}
}

// TestMediaPlaylist_PreLiveEdgeReturns503 asserts that a fetch before
// any playlist is renderable returns 503 — both for the no-tag path
// and the sticky-URL path — so a player follows the existing retry
// loop until a snapshot is available.
func TestMediaPlaylist_PreLiveEdgeReturns503(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _ := testStreamRouter(t, clk)
	initStream(t, store, "1")
	// No commits — handler will block on HasPlaylist, then the
	// request context cancellation surfaces as 503.
	for _, path := range []string{"/stream/1/media.m3u8", "/stream/1/media.m3u8?to=6.000"} {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		cancel()
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "path=%s", path)
		assert.Equal(t, "2", rec.Header().Get("Retry-After"), "path=%s", path)
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
