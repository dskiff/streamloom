package routes

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/dskiff/streamloom/pkg/config"
	"github.com/dskiff/streamloom/pkg/stream"
	"github.com/dskiff/streamloom/pkg/watcher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- GET /stream/{streamID}/stream.m3u8 (master playlist) tests ---

func TestMasterPlaylist_Success(t *testing.T) {
	streamRouter, apiRouter, store, _ := testBothRoutersWithToken(t, clock.Real{})

	hdrs := initHeaders()
	rec := postInit(apiRouter, "1", "test-token", hdrs, []byte("init-data"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	req := httptest.NewRequest(http.MethodGet, "/stream/1/stream.m3u8", nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, config.M3U8_MIME_TYPE, rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	assert.NotEmpty(t, rec.Header().Get("Content-Length"))

	body := rec.Body.String()
	assert.Contains(t, body, "#EXTM3U")
	assert.Contains(t, body, "#EXT-X-VERSION:7")
	assert.Contains(t, body, "BANDWIDTH=4000000")
	assert.Contains(t, body, "RESOLUTION=1920x1080")
	assert.Contains(t, body, `CODECS="avc1.64001f"`)
	assert.Contains(t, body, "FRAME-RATE=23.976")
	assert.Contains(t, body, "media.m3u8")
}

func TestMasterPlaylist_StreamNotFound(t *testing.T) {
	streamRouter, _, _, _ := testBothRoutersWithToken(t, clock.Real{})

	// Valid-format but unknown stream ID returns 503 (not 404) to prevent
	// stream ID enumeration via response-code differentiation.
	req := httptest.NewRequest(http.MethodGet, "/stream/999/stream.m3u8", nil)
	rec := httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))
}

// --- End-to-end: init -> push -> retrieve ---

func TestE2E_InitPushRetrieve(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	streamRouter, apiRouter, store, _ := testBothRoutersWithToken(t, clk)

	// 1. Init the stream via the HTTP API.
	hdrs := initHeaders()
	rec := postInit(apiRouter, "1", "test-token", hdrs, []byte("init-data"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	// 2. Advance time so the segment is immediately eligible for the playlist
	// renderer. The renderer uses real time.Timer sleeps but checks clock.Now()
	// for eligibility, so the mock time must be >= segment timestamp before
	// the commit notification wakes the renderer.
	clk.Set(time.UnixMilli(10000))

	// 3. Push a segment via the HTTP API.
	segData := []byte("hello-segment")
	rec = postSegment(apiRouter, "1", "test-token", "0", "5000", "2000", segData)
	require.Equal(t, http.StatusCreated, rec.Code)

	// 4. Retrieve init.mp4 using the timestamped URL via stream server.
	s := store.Get("1")
	require.NotNil(t, s)
	initURL := fmt.Sprintf("/stream/1/init-%d.mp4", s.InitTimestampMs())
	req := httptest.NewRequest(http.MethodGet, initURL, nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []byte("init-data"), rec.Body.Bytes())

	// 5. Retrieve the segment via stream server.
	req = httptest.NewRequest(http.MethodGet, "/stream/1/segment_0.m4s", nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, segData, rec.Body.Bytes())

	// 6. Verify media playlist via stream server.
	require.Eventually(t, func() bool {
		p := s.CachedPlaylist()
		return p != "" && strings.Contains(p, "segment_0.m4s")
	}, 2*time.Second, 10*time.Millisecond)

	req = httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "segment_0.m4s")
}

func TestE2E_StringStreamID(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))

	store := stream.NewStore(clk)
	l := slog.Default()
	env := config.Env{
		STREAM_MAX_BUFFER_BYTES: config.DefaultStreamMaxBufferBytes,
		BUFFER_WORKING_SPACE:    config.DefaultBufferWorkingSpace,
		STREAM_TOKENS: map[string]config.TokenDigest{
			"myStream": sha256.Sum256([]byte("Bearer my-token")),
		},
	}
	tracker := watcher.NewTracker(clk)
	streamRouter := Stream(l, env, store, nil, tracker)
	apiRouter := API(l, env, store, nil, tracker)

	// 1. Init the stream with a non-numeric string ID.
	hdrs := initHeaders()
	rec := postInit(apiRouter, "myStream", "my-token", hdrs, []byte("init-data"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("myStream") })

	clk.Set(time.UnixMilli(10000))

	// 2. Push a segment.
	segData := []byte("string-id-segment")
	rec = postSegment(apiRouter, "myStream", "my-token", "0", "5000", "2000", segData)
	require.Equal(t, http.StatusCreated, rec.Code)

	// 3. Retrieve init.mp4.
	s := store.Get("myStream")
	require.NotNil(t, s)
	initURL := fmt.Sprintf("/stream/myStream/init-%d.mp4", s.InitTimestampMs())
	req := httptest.NewRequest(http.MethodGet, initURL, nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []byte("init-data"), rec.Body.Bytes())

	// 4. Retrieve the segment.
	req = httptest.NewRequest(http.MethodGet, "/stream/myStream/segment_0.m4s", nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, segData, rec.Body.Bytes())

	// 5. Verify media playlist.
	require.Eventually(t, func() bool {
		p := s.CachedPlaylist()
		return p != "" && strings.Contains(p, "segment_0.m4s")
	}, 2*time.Second, 10*time.Millisecond)

	req = httptest.NewRequest(http.MethodGet, "/stream/myStream/media.m3u8", nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "segment_0.m4s")
}
