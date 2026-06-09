package routes

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/dskiff/streamloom/pkg/config"
	"github.com/dskiff/streamloom/pkg/stream"
	"github.com/dskiff/streamloom/pkg/watcher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testRoutersWithTrustedProxy builds stream + API routers that trust
// 10.0.0.0/8 as a reverse-proxy range, so X-Forwarded-For presented by a 10.x
// peer is honored. Used to exercise client-IP spoof-resistance end to end.
func testRoutersWithTrustedProxy(t *testing.T, clk clock.Clock) (streamRouter, apiRouter http.Handler, store *stream.Store) {
	t.Helper()
	store = stream.NewStore(clk)
	tracker := watcher.NewTracker(clk)
	l := slog.Default()
	_, cidr, err := net.ParseCIDR("10.0.0.0/8")
	require.NoError(t, err)
	env := config.Env{
		STREAM_MAX_BUFFER_BYTES: config.DefaultStreamMaxBufferBytes,
		BUFFER_WORKING_SPACE:    config.DefaultBufferWorkingSpace,
		TRUSTED_PROXIES:         []*net.IPNet{cidr},
		STREAM_TOKENS: map[string]config.TokenDigest{
			"1": sha256.Sum256([]byte("Bearer test-token")),
		},
	}
	streamRouter = Stream(l, env, store, nil, tracker)
	apiRouter = API(l, env, store, nil, tracker)
	return streamRouter, apiRouter, store
}

func queryActiveWatchers(t *testing.T, apiRouter http.Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream/1/active_watchers?window_ms=60000", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

// A single client behind the trusted proxy cannot inflate the active-watcher
// count by cycling True-Client-IP: the header is ignored, so every request
// collapses to the real forwarded client IP.
func TestActiveWatchers_TrueClientIPSpoofDoesNotInflate(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	streamRouter, apiRouter, store := testRoutersWithTrustedProxy(t, clk)
	initStream(t, store, "1")

	for i := 0; i < 50; i++ {
		req := httptest.NewRequest(http.MethodGet, "/stream/1/stream.m3u8", nil)
		req.RemoteAddr = "10.0.0.9:1234"                                 // the trusted proxy
		req.Header.Set("X-Forwarded-For", "198.51.100.7")                // real client
		req.Header.Set("True-Client-IP", fmt.Sprintf("203.0.113.%d", i)) // spoof attempt
		streamRouter.ServeHTTP(httptest.NewRecorder(), req)
	}

	assert.Equal(t, "1", queryActiveWatchers(t, apiRouter),
		"spoofed True-Client-IP must not create distinct watchers")
}

// Distinct real clients forwarded by the trusted proxy are still counted
// distinctly, confirming the spoof fix does not break legitimate counting.
func TestActiveWatchers_DistinctForwardedClientsCounted(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	streamRouter, apiRouter, store := testRoutersWithTrustedProxy(t, clk)
	initStream(t, store, "1")

	for _, c := range []string{"198.51.100.1", "198.51.100.2", "198.51.100.3"} {
		req := httptest.NewRequest(http.MethodGet, "/stream/1/stream.m3u8", nil)
		req.RemoteAddr = "10.0.0.9:1234"
		req.Header.Set("X-Forwarded-For", c)
		streamRouter.ServeHTTP(httptest.NewRecorder(), req)
	}

	assert.Equal(t, "3", queryActiveWatchers(t, apiRouter))
}

// A client that prepends a forged X-Forwarded-For entry cannot mint distinct
// watchers: only the rightmost (proxy-appended) entry is honored.
func TestActiveWatchers_ForgedForwardedPrefixDoesNotInflate(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	streamRouter, apiRouter, store := testRoutersWithTrustedProxy(t, clk)
	initStream(t, store, "1")

	for i := 0; i < 25; i++ {
		req := httptest.NewRequest(http.MethodGet, "/stream/1/stream.m3u8", nil)
		req.RemoteAddr = "10.0.0.9:1234"
		// Forged leftmost entries vary per request; the proxy-appended peer
		// (rightmost) is the same real client every time.
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d, 198.51.100.7", i))
		streamRouter.ServeHTTP(httptest.NewRecorder(), req)
	}

	assert.Equal(t, "1", queryActiveWatchers(t, apiRouter),
		"forged leftmost X-Forwarded-For entries must not create distinct watchers")
}

// Watchers are recorded only for streams the store holds. A request to a
// configured-but-not-yet-initialized stream must not seed a tracker entry that
// would surface once the stream goes live.
func TestActiveWatchers_UninitializedStreamNotRecorded(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	streamRouter, apiRouter, store, _ := testBothRoutersWithToken(t, clk)

	// Hit the stream route before it is initialized.
	req := httptest.NewRequest(http.MethodGet, "/stream/1/stream.m3u8", nil)
	req.RemoteAddr = "203.0.113.7:1234"
	rec := httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code,
		"an uninitialized stream serves 503")

	// Initialize and query: the earlier hit must not be counted.
	initStream(t, store, "1")
	assert.Equal(t, "0", queryActiveWatchers(t, apiRouter),
		"a pre-init request must not have been recorded")
}
