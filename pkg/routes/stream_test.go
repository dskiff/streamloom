package routes

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/dskiff/streamloom/pkg/config"
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

// --- GET /stream/{streamID}/init-{ts}.mp4 tests ---

func TestGetInitMP4_Success(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	router, store, _ := testStreamRouter(t, clk)
	initStream(t, store, "1")

	s := store.Get("1")
	require.NotNil(t, s)
	initURL := fmt.Sprintf("/stream/1/init-%d.mp4", s.InitTimestampMs())

	req := httptest.NewRequest(http.MethodGet, initURL, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, config.MP4_MIME_TYPE, rec.Header().Get("Content-Type"))
	assert.Equal(t, "public, max-age=604800, immutable", rec.Header().Get("Cache-Control"))
	assert.Equal(t, strconv.Itoa(len("init-data")), rec.Header().Get("Content-Length"))
	assert.Equal(t, []byte("init-data"), rec.Body.Bytes())
}

func TestGetInitMP4_UnconfiguredStream_Returns503(t *testing.T) {
	router, _, _ := testStreamRouter(t, clock.Real{})

	// Unconfigured streams with valid IDs return 503 to prevent enumeration.
	req := httptest.NewRequest(http.MethodGet, "/stream/999/init-123.mp4", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))
}

func TestGetInitMP4_ConfiguredButUninitialized_Returns503(t *testing.T) {
	router, _, _ := testStreamRouter(t, clock.Real{})

	// Stream 1 is configured but not initialized.
	req := httptest.NewRequest(http.MethodGet, "/stream/1/init-123.mp4", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))
}

func TestGetInitMP4_StaleInitID(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	router, store, _ := testStreamRouter(t, clk)
	initStream(t, store, "1")

	// Request with a mismatched initID should return 404.
	req := httptest.NewRequest(http.MethodGet, "/stream/1/init-9999.mp4", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestGetInitMP4_InvalidInitID(t *testing.T) {
	router, store, _ := testStreamRouter(t, clock.Real{})
	initStream(t, store, "1")

	req := httptest.NewRequest(http.MethodGet, "/stream/1/init-abc.mp4", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
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
	assert.Contains(t, body, "#EXT-X-MAP:URI=\"init-0.mp4\"")
	assert.Contains(t, body, "segment_0.m4s")
	assert.Contains(t, body, "segment_1.m4s")
	assert.Contains(t, body, "segment_2.m4s")
	assert.Contains(t, body, "#EXTINF:2.000,")
	assert.Contains(t, body, "#EXT-X-PROGRAM-DATE-TIME:")
}

func TestMediaPlaylist_WallClockFiltering(t *testing.T) {
	// Start at time 0 so all segment commits are accepted.
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _ := testStreamRouter(t, clk)
	initStream(t, store, "1")

	s := store.Get("1")
	require.NotNil(t, s)

	// Segments: 0 at t=1000, 1 at t=3000, 2 at t=5000, 3 at t=7000
	commitSegment(t, s, 0, []byte("seg0"), 1000)
	commitSegment(t, s, 1, []byte("seg1"), 3000)
	commitSegment(t, s, 2, []byte("seg2"), 5000)
	commitSegment(t, s, 3, []byte("seg3"), 7000)

	// Advance time to 5000: segments 0,1,2 eligible, segment 3 is future.
	clk.Set(time.UnixMilli(5000))

	// Wait for renderer to populate.
	require.Eventually(t, func() bool {
		return s.CachedPlaylist() != ""
	}, 2*time.Second, 10*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	// Past + at-now segments should be present.
	assert.Contains(t, body, "segment_0.m4s")
	assert.Contains(t, body, "segment_1.m4s")
	assert.Contains(t, body, "segment_2.m4s")
	// Future segment should NOT be present.
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
	// when the mock clock moves backward so all segments become future.
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _ := testStreamRouter(t, clk)
	initStream(t, store, "1")

	s := store.Get("1")
	require.NotNil(t, s)

	// Commit segments in the future and advance clock so they become eligible.
	commitSegment(t, s, 0, []byte("seg0"), 2000)
	commitSegment(t, s, 1, []byte("seg1"), 4000)
	clk.Set(time.UnixMilli(5000))

	require.Eventually(t, func() bool {
		return s.CachedPlaylist() != ""
	}, 2*time.Second, 10*time.Millisecond)

	// Move the clock backward so all segments are now in the future.
	// The renderer will re-render and produce an empty playlist while
	// hasPlaylist remains closed.
	clk.Set(time.UnixMilli(0))

	// Poke the renderer to re-render by committing a future segment.
	commitSegment(t, s, 2, []byte("seg2"), 6000)

	require.Eventually(t, func() bool {
		return s.CachedPlaylist() == ""
	}, 2*time.Second, 10*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))
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
	initURL := fmt.Sprintf("/stream/1/init-%d.mp4", s.InitTimestampMs())
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
