package routes

import (
	"bytes"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/dskiff/streamloom/pkg/config"
	"github.com/dskiff/streamloom/pkg/stream"
	"github.com/stretchr/testify/require"
)

// testStreamRouter creates a stream router with a pre-populated store.
// The returned store can be used to initialize streams before making requests.
func testStreamRouter(t *testing.T, clk clock.Clock) (http.Handler, *stream.Store) {
	t.Helper()
	store := stream.NewStore(clk)
	l := slog.Default()
	env := config.Env{
		STREAM_MAX_BUFFER_BYTES: config.DefaultStreamMaxBufferBytes,
		BUFFER_WORKING_SPACE:    config.DefaultBufferWorkingSpace,
		STREAM_TOKENS: map[string]config.TokenDigest{
			"1": sha256.Sum256([]byte("Bearer test-token")),
		},
	}
	router := Stream(l, env, store, nil)
	return router, store
}

// testAPIRouterWithToken creates an API router with a configured token for stream 1.
func testAPIRouterWithToken(t *testing.T, clk clock.Clock) (http.Handler, *stream.Store, config.Env) {
	t.Helper()
	store := stream.NewStore(clk)
	l := slog.Default()
	env := config.Env{
		STREAM_MAX_BUFFER_BYTES: config.DefaultStreamMaxBufferBytes,
		BUFFER_WORKING_SPACE:    config.DefaultBufferWorkingSpace,
		STREAM_TOKENS: map[string]config.TokenDigest{
			"1": sha256.Sum256([]byte("Bearer test-token")),
		},
	}
	router := API(l, env, store, nil)
	return router, store, env
}

// testBothRoutersWithToken creates both stream and API routers sharing a store.
// Used for E2E tests that push via API and read via stream server.
func testBothRoutersWithToken(t *testing.T, clk clock.Clock) (streamRouter http.Handler, apiRouter http.Handler, store *stream.Store) {
	t.Helper()
	store = stream.NewStore(clk)
	l := slog.Default()
	env := config.Env{
		STREAM_MAX_BUFFER_BYTES: config.DefaultStreamMaxBufferBytes,
		BUFFER_WORKING_SPACE:    config.DefaultBufferWorkingSpace,
		STREAM_TOKENS: map[string]config.TokenDigest{
			"1": sha256.Sum256([]byte("Bearer test-token")),
		},
	}
	streamRouter = Stream(l, env, store, nil)
	apiRouter = API(l, env, store, nil)
	return streamRouter, apiRouter, store
}

// initStream initializes a test stream in the store with a known init segment.
// Registers a cleanup to delete the stream and stop its renderer goroutine.
func initStream(t *testing.T, store *stream.Store, id string) {
	t.Helper()
	meta := stream.Metadata{
		Bandwidth:          4000000,
		Codecs:             "avc1.64001f",
		Width:              1920,
		Height:             1080,
		FrameRate:          23.976,
		TargetDurationSecs: 2,
	}
	err := store.Init(id, meta, []byte("init-data"), 10, 1024, 5, 2, config.DefaultMediaWindowSize)
	require.NoError(t, err)
	t.Cleanup(func() { store.Delete(id) })
}

// commitSegment adds a segment to the stream with the given index and data.
// On commit failure the slot is released back to the pool to avoid leaking capacity.
func commitSegment(t *testing.T, s *stream.Stream, index uint32, data []byte, tsMs int64) {
	t.Helper()
	buf, ok := s.AcquireSlot()
	require.True(t, ok, "AcquireSlot should succeed")
	_, err := buf.ReadFrom(bytes.NewReader(data))
	require.NoError(t, err)
	err = s.CommitSlot(index, buf, tsMs, 2000)
	if err != nil {
		s.ReleaseSlot(buf)
	}
	require.NoError(t, err)
}

// initHeaders returns the minimum set of valid init headers.
func initHeaders() map[string]string {
	return map[string]string{
		"X-SL-BANDWIDTH":            "4000000",
		"X-SL-CODECS":               "avc1.64001f",
		"X-SL-RESOLUTION":           "1920x1080",
		"X-SL-FRAMERATE":            "23.976",
		"X-SL-TARGET-DURATION":      "2",
		"X-SL-SEGMENT-CAP":          "10",
		"X-SL-SEGMENT-BYTES":        "1024",
		"X-SL-BACKWARD-BUFFER-SIZE": "5",
	}
}

func postInit(router http.Handler, streamID string, token string, headers map[string]string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/stream/"+streamID+"/init", bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func postSegment(router http.Handler, streamID string, token string, index, timestamp, durationMs string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/stream/"+streamID+"/segment", bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if index != "" {
		req.Header.Set("X-SL-INDEX", index)
	}
	if timestamp != "" {
		req.Header.Set("X-SL-TIMESTAMP", timestamp)
	}
	if durationMs != "" {
		req.Header.Set("X-SL-DURATION", durationMs)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}
