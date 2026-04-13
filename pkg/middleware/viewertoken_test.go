package middleware

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/dskiff/streamloom/pkg/viewer"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// okHandler always returns 200 OK; it's the downstream handler under test.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
})

func newTestRouter(clk clock.Clock, keys map[string][]byte) http.Handler {
	r := chi.NewRouter()
	r.Route("/stream/{streamID}", func(sr chi.Router) {
		sr.Use(ViewerTokenAuth(clk, keys, slog.Default()))
		sr.Get("/thing", okHandler.ServeHTTP)
	})
	return r
}

func TestViewerTokenAuth_NoKeyConfigured_PassesThrough(t *testing.T) {
	r := newTestRouter(clock.Real{}, map[string][]byte{})
	req := httptest.NewRequest(http.MethodGet, "/stream/1/thing", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestViewerTokenAuth_InvalidStreamID_NotFound(t *testing.T) {
	r := newTestRouter(clock.Real{}, map[string][]byte{"1": []byte("key")})
	req := httptest.NewRequest(http.MethodGet, "/stream/a.b/thing", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestViewerTokenAuth_ValidToken_PassesThrough(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := viewer.Mint(key, clk.Now().Add(time.Hour).UnixMilli())
	require.NoError(t, err)

	r := newTestRouter(clk, map[string][]byte{"1": key})
	req := httptest.NewRequest(http.MethodGet, "/stream/1/thing?vt="+tok, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestViewerTokenAuth_MissingToken_401(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	key := []byte("0123456789abcdef0123456789abcdef")

	r := newTestRouter(clk, map[string][]byte{"1": key})
	req := httptest.NewRequest(http.MethodGet, "/stream/1/thing", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestViewerTokenAuth_ExpiredToken_401(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	key := []byte("0123456789abcdef0123456789abcdef")
	// Mint with exp in the past relative to clk.
	tok, err := viewer.Mint(key, clk.Now().Add(-1*time.Second).UnixMilli())
	require.NoError(t, err)

	r := newTestRouter(clk, map[string][]byte{"1": key})
	req := httptest.NewRequest(http.MethodGet, "/stream/1/thing?vt="+tok, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestViewerTokenAuth_TamperedToken_401(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := viewer.Mint(key, clk.Now().Add(time.Hour).UnixMilli())
	require.NoError(t, err)
	// Flip the last character.
	bad := tok[:len(tok)-1] + "A"
	if bad == tok {
		bad = tok[:len(tok)-1] + "B"
	}

	r := newTestRouter(clk, map[string][]byte{"1": key})
	req := httptest.NewRequest(http.MethodGet, "/stream/1/thing?vt="+bad, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestViewerTokenAuth_WrongStreamKey_401(t *testing.T) {
	// Two streams, different keys. A token minted for stream 1 must not
	// authorize requests to stream 2.
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	key1 := []byte("0123456789abcdef0123456789abcdef")
	key2 := []byte("ffffffffffffffffffffffffffffffff")
	tok, err := viewer.Mint(key1, clk.Now().Add(time.Hour).UnixMilli())
	require.NoError(t, err)

	r := newTestRouter(clk, map[string][]byte{"1": key1, "2": key2})
	req := httptest.NewRequest(http.MethodGet, "/stream/2/thing?vt="+tok, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
