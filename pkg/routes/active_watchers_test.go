package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActiveWatchers_NoAuth_Returns401(t *testing.T) {
	_, apiRouter, _, _ := testBothRoutersWithToken(t, clock.Real{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/1/active_watchers", nil)
	rec := httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestActiveWatchers_InvalidStreamID_Returns404(t *testing.T) {
	_, apiRouter, _, _ := testBothRoutersWithToken(t, clock.Real{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/a.b/active_watchers", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestActiveWatchers_NoWatchers_ReturnsZero(t *testing.T) {
	_, apiRouter, _, _ := testBothRoutersWithToken(t, clock.Real{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/1/active_watchers?window_ms=60000", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/plain", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "0", rec.Body.String())
}

func TestActiveWatchers_CountsDistinctIPs(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	streamRouter, apiRouter, store, _ := testBothRoutersWithToken(t, clk)
	initStream(t, store, "1")

	// Simulate requests from different IPs to the stream server.
	for _, ip := range []string{"10.0.0.1:1234", "10.0.0.2:1234", "10.0.0.3:1234"} {
		req := httptest.NewRequest(http.MethodGet, "/stream/1/stream.m3u8", nil)
		req.RemoteAddr = ip
		rec := httptest.NewRecorder()
		streamRouter.ServeHTTP(rec, req)
	}

	// Query active watchers.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/1/active_watchers?window_ms=60000", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "3", rec.Body.String())
}

func TestActiveWatchers_SameIPCountedOnce(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	streamRouter, apiRouter, store, _ := testBothRoutersWithToken(t, clk)
	initStream(t, store, "1")

	// Same IP requests multiple times.
	for range 5 {
		req := httptest.NewRequest(http.MethodGet, "/stream/1/stream.m3u8", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		rec := httptest.NewRecorder()
		streamRouter.ServeHTTP(rec, req)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/1/active_watchers?window_ms=60000", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "1", rec.Body.String())
}

func TestActiveWatchers_WindowFiltersOldRequests(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	streamRouter, apiRouter, store, _ := testBothRoutersWithToken(t, clk)
	initStream(t, store, "1")

	// IP 1 requests at t=1000.
	req := httptest.NewRequest(http.MethodGet, "/stream/1/stream.m3u8", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	streamRouter.ServeHTTP(httptest.NewRecorder(), req)

	// Advance time.
	clk.Set(time.UnixMilli(70000))

	// IP 2 requests at t=70000.
	req = httptest.NewRequest(http.MethodGet, "/stream/1/stream.m3u8", nil)
	req.RemoteAddr = "10.0.0.2:1234"
	streamRouter.ServeHTTP(httptest.NewRecorder(), req)

	// Query with 30s window — only IP 2 should be counted.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/stream/1/active_watchers?window_ms=30000", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "1", rec.Body.String())
}

func TestActiveWatchers_WindowCappedAt60Min(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	streamRouter, apiRouter, store, _ := testBothRoutersWithToken(t, clk)
	initStream(t, store, "1")

	// Request at t=1000.
	req := httptest.NewRequest(http.MethodGet, "/stream/1/stream.m3u8", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	streamRouter.ServeHTTP(httptest.NewRecorder(), req)

	// Advance past 60 minutes.
	clk.Set(time.UnixMilli(1000 + 3_600_001))

	// Even with a huge window, the cap should exclude the old entry.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/stream/1/active_watchers?window_ms=99999999", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "0", rec.Body.String())
}

func TestActiveWatchers_DefaultWindow(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	streamRouter, apiRouter, store, _ := testBothRoutersWithToken(t, clk)
	initStream(t, store, "1")

	req := httptest.NewRequest(http.MethodGet, "/stream/1/stream.m3u8", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	streamRouter.ServeHTTP(httptest.NewRecorder(), req)

	// Without window_ms param, should use default (60s).
	req = httptest.NewRequest(http.MethodGet, "/api/v1/stream/1/active_watchers", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "1", rec.Body.String())

	// Advance past default window.
	clk.Set(time.UnixMilli(1000 + 60_001))

	req = httptest.NewRequest(http.MethodGet, "/api/v1/stream/1/active_watchers", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec = httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "0", rec.Body.String())
}

func TestActiveWatchers_InvalidWindowMs_Returns400(t *testing.T) {
	_, apiRouter, _, _ := testBothRoutersWithToken(t, clock.Real{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/1/active_watchers?window_ms=abc", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestActiveWatchers_NegativeWindowMs_Returns400(t *testing.T) {
	_, apiRouter, _, _ := testBothRoutersWithToken(t, clock.Real{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/1/active_watchers?window_ms=-1", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestActiveWatchers_ZeroWindowMs_Returns400(t *testing.T) {
	_, apiRouter, _, _ := testBothRoutersWithToken(t, clock.Real{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/1/active_watchers?window_ms=0", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestActiveWatchers_DeleteStreamClearsWatchers(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	streamRouter, apiRouter, store, _ := testBothRoutersWithToken(t, clk)
	initStream(t, store, "1")

	// Record a watcher.
	req := httptest.NewRequest(http.MethodGet, "/stream/1/stream.m3u8", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	streamRouter.ServeHTTP(httptest.NewRecorder(), req)

	// Delete the stream via API.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/stream/1/", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)

	// Re-init so we can query active_watchers again.
	initStream(t, store, "1")

	// Watchers should be cleared after delete.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/stream/1/active_watchers?window_ms=60000", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec = httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "0", rec.Body.String())
}
