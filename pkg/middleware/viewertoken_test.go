package middleware

import (
	"encoding/base64"
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

// newTestRouter mounts ViewerTokenAuth on a single route; tests that do not
// exercise type scoping can accept any token class.
func newTestRouter(clk clock.Clock, keys map[string][]byte) http.Handler {
	return newTestRouterWithTypes(clk, keys, viewer.TypeViewer, viewer.TypeSegment)
}

// newTestRouterWithTypes mounts ViewerTokenAuth with an explicit allowed-type
// set so tests can assert per-route scoping behavior.
func newTestRouterWithTypes(clk clock.Clock, keys map[string][]byte, allowed ...viewer.Type) http.Handler {
	r := chi.NewRouter()
	r.Route("/stream/{streamID}", func(sr chi.Router) {
		sr.Use(ViewerTokenAuth(clk, keys, slog.Default(), allowed...))
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
	tok, err := viewer.Mint(key, clk.Now().Add(time.Hour).UnixMilli(), viewer.TypeViewer)
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
	tok, err := viewer.Mint(key, clk.Now().Add(-1*time.Second).UnixMilli(), viewer.TypeViewer)
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
	tok, err := viewer.Mint(key, clk.Now().Add(time.Hour).UnixMilli(), viewer.TypeViewer)
	require.NoError(t, err)
	// Tamper at the byte level (decode → flip → re-encode) rather than at
	// the base64 character level. The 22-byte payload encodes to 30
	// base64url characters, 4 of which are excess padding bits, so a
	// character flip may land on unused bits and leave the decoded
	// payload (and therefore the MAC) intact.
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	require.NoError(t, err)
	raw[len(raw)-1] ^= 0x01
	bad := base64.RawURLEncoding.EncodeToString(raw)

	r := newTestRouter(clk, map[string][]byte{"1": key})
	req := httptest.NewRequest(http.MethodGet, "/stream/1/thing?vt="+bad, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestViewerTokenAuth_TypeNotAllowed_401 asserts that a cryptographically
// valid token whose Type is not in the route's allowed-set is rejected. This
// is the mechanism that prevents a TypeSegment token (baked into playlist
// URIs) from being replayed on a playlist-scoped route to rotate into a fresh
// token and defeat the TTL.
func TestViewerTokenAuth_TypeNotAllowed_401(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := viewer.Mint(key, clk.Now().Add(time.Hour).UnixMilli(), viewer.TypeSegment)
	require.NoError(t, err)

	// Playlist-scoped router: TypeViewer only.
	r := newTestRouterWithTypes(clk, map[string][]byte{"1": key}, viewer.TypeViewer)
	req := httptest.NewRequest(http.MethodGet, "/stream/1/thing?vt="+tok, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestViewerTokenAuth_TypeAllowed_PassesThrough asserts that a TypeSegment
// token IS accepted on a route whose allowed-set includes TypeSegment (e.g.
// the init/segment routes).
func TestViewerTokenAuth_TypeAllowed_PassesThrough(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := viewer.Mint(key, clk.Now().Add(time.Hour).UnixMilli(), viewer.TypeSegment)
	require.NoError(t, err)

	r := newTestRouterWithTypes(clk, map[string][]byte{"1": key}, viewer.TypeViewer, viewer.TypeSegment)
	req := httptest.NewRequest(http.MethodGet, "/stream/1/thing?vt="+tok, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestViewerTokenAuth_NoAllowedTypes_Panics asserts constructor-time refusal
// of an empty allowed-types set. A permission-less route group would silently
// reject all traffic; failing loudly at wiring time is strictly better.
func TestViewerTokenAuth_NoAllowedTypes_Panics(t *testing.T) {
	assert.Panics(t, func() {
		_ = ViewerTokenAuth(clock.Real{}, map[string][]byte{}, slog.Default())
	})
}

func TestViewerTokenAuth_WrongStreamKey_401(t *testing.T) {
	// Two streams, different keys. A token minted for stream 1 must not
	// authorize requests to stream 2.
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	key1 := []byte("0123456789abcdef0123456789abcdef")
	key2 := []byte("ffffffffffffffffffffffffffffffff")
	tok, err := viewer.Mint(key1, clk.Now().Add(time.Hour).UnixMilli(), viewer.TypeViewer)
	require.NoError(t, err)

	r := newTestRouter(clk, map[string][]byte{"1": key1, "2": key2})
	req := httptest.NewRequest(http.MethodGet, "/stream/2/thing?vt="+tok, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
