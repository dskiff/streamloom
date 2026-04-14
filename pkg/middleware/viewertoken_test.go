package middleware

import (
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/dskiff/streamloom/pkg/config"
	"github.com/dskiff/streamloom/pkg/viewer"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// okHandler always returns 200 OK; it's the downstream handler under test.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
})

// testEnvSecret is a fixed env-var-style secret used to derive both class
// keys in the middleware tests.
const testEnvSecret = "0123456789abcdef0123456789abcdef"

// newKeysMap builds a ViewerKeys entry for streamID "1" under the shared
// testEnvSecret. Tests that want a mismatched-key scenario derive their
// own ViewerKeys from a different secret.
func newKeysMap(t *testing.T, streamID string) map[string]config.ViewerKeys {
	t.Helper()
	pk, err := viewer.DeriveKey([]byte(testEnvSecret), streamID, viewer.TypePlaylist)
	require.NoError(t, err)
	sk, err := viewer.DeriveKey([]byte(testEnvSecret), streamID, viewer.TypeSegment)
	require.NoError(t, err)
	return map[string]config.ViewerKeys{streamID: {Playlist: pk, Segment: sk}}
}

// newTestRouter mounts ViewerTokenAuth on a single route accepting both
// class keys (init/segment-style scoping).
func newTestRouter(clk clock.Clock, keys map[string]config.ViewerKeys) http.Handler {
	return newTestRouterWithTypes(clk, keys, viewer.TypeSegment, viewer.TypePlaylist)
}

// newTestRouterWithTypes mounts ViewerTokenAuth with an explicit allowed-type
// set so tests can assert per-route scoping behavior.
func newTestRouterWithTypes(clk clock.Clock, keys map[string]config.ViewerKeys, allowed ...viewer.Type) http.Handler {
	r := chi.NewRouter()
	r.Route("/stream/{streamID}", func(sr chi.Router) {
		sr.Use(ViewerTokenAuth(clk, keys, slog.Default(), allowed...))
		sr.Get("/thing", okHandler.ServeHTTP)
	})
	return r
}

func TestViewerTokenAuth_NoKeyConfigured_PassesThrough(t *testing.T) {
	r := newTestRouter(clock.Real{}, map[string]config.ViewerKeys{})
	req := httptest.NewRequest(http.MethodGet, "/stream/1/thing", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestViewerTokenAuth_InvalidStreamID_NotFound(t *testing.T) {
	r := newTestRouter(clock.Real{}, newKeysMap(t, "1"))
	req := httptest.NewRequest(http.MethodGet, "/stream/a.b/thing", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestViewerTokenAuth_ValidToken_PassesThrough(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	keys := newKeysMap(t, "1")
	tok, err := viewer.Mint(keys["1"].Playlist, clk.Now().Add(time.Hour).UnixMilli())
	require.NoError(t, err)

	r := newTestRouter(clk, keys)
	req := httptest.NewRequest(http.MethodGet, "/stream/1/thing?vt="+tok, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestViewerTokenAuth_MissingToken_401(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))

	r := newTestRouter(clk, newKeysMap(t, "1"))
	req := httptest.NewRequest(http.MethodGet, "/stream/1/thing", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestViewerTokenAuth_ExpiredToken_401(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	keys := newKeysMap(t, "1")
	// Mint with exp in the past relative to clk.
	tok, err := viewer.Mint(keys["1"].Playlist, clk.Now().Add(-1*time.Second).UnixMilli())
	require.NoError(t, err)

	r := newTestRouter(clk, keys)
	req := httptest.NewRequest(http.MethodGet, "/stream/1/thing?vt="+tok, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestViewerTokenAuth_TamperedToken_401(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	keys := newKeysMap(t, "1")
	tok, err := viewer.Mint(keys["1"].Playlist, clk.Now().Add(time.Hour).UnixMilli())
	require.NoError(t, err)
	// Tamper at the byte level (decode → flip → re-encode). With a
	// 21-byte payload encoded to 28 base64url chars there are no unused
	// trailing bits, but decoding and re-encoding works uniformly.
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	require.NoError(t, err)
	raw[len(raw)-1] ^= 0x01
	bad := base64.RawURLEncoding.EncodeToString(raw)

	r := newTestRouter(clk, keys)
	req := httptest.NewRequest(http.MethodGet, "/stream/1/thing?vt="+bad, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestViewerTokenAuth_SegmentTokenOnPlaylistRoute_401 asserts that a
// token minted with the segment-derived key is rejected on a route that
// only allows TypePlaylist. This is the mechanism (now KDF-backed rather
// than type-byte-backed) that prevents a renderer-baked segment token
// from being replayed on a playlist route to rotate into a fresh token
// and defeat the TTL.
func TestViewerTokenAuth_SegmentTokenOnPlaylistRoute_401(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	keys := newKeysMap(t, "1")
	tok, err := viewer.Mint(keys["1"].Segment, clk.Now().Add(time.Hour).UnixMilli())
	require.NoError(t, err)

	// Playlist-scoped router: TypePlaylist only.
	r := newTestRouterWithTypes(clk, keys, viewer.TypePlaylist)
	req := httptest.NewRequest(http.MethodGet, "/stream/1/thing?vt="+tok, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestViewerTokenAuth_SegmentTokenOnSegmentRoute_PassesThrough asserts a
// segment-class token is accepted on a route whose allowed set includes
// TypeSegment (e.g. init/segment routes).
func TestViewerTokenAuth_SegmentTokenOnSegmentRoute_PassesThrough(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	keys := newKeysMap(t, "1")
	tok, err := viewer.Mint(keys["1"].Segment, clk.Now().Add(time.Hour).UnixMilli())
	require.NoError(t, err)

	r := newTestRouterWithTypes(clk, keys, viewer.TypeSegment, viewer.TypePlaylist)
	req := httptest.NewRequest(http.MethodGet, "/stream/1/thing?vt="+tok, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestViewerTokenAuth_PlaylistTokenOnSegmentRoute_PassesThrough asserts
// the operator-grant (playlist-class) token still works on segment routes
// — the refactor preserves today's behavior that an operator token can
// fetch both playlists and segments.
func TestViewerTokenAuth_PlaylistTokenOnSegmentRoute_PassesThrough(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	keys := newKeysMap(t, "1")
	tok, err := viewer.Mint(keys["1"].Playlist, clk.Now().Add(time.Hour).UnixMilli())
	require.NoError(t, err)

	r := newTestRouterWithTypes(clk, keys, viewer.TypeSegment, viewer.TypePlaylist)
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
		_ = ViewerTokenAuth(clock.Real{}, map[string]config.ViewerKeys{}, slog.Default())
	})
}

// TestViewerTokenAuth_UnknownType_Panics asserts constructor-time refusal
// of an unrecognized viewer.Type. This is a programmer-error guard — no
// external input reaches the middleware with an unknown Type.
func TestViewerTokenAuth_UnknownType_Panics(t *testing.T) {
	assert.Panics(t, func() {
		_ = ViewerTokenAuth(clock.Real{}, map[string]config.ViewerKeys{}, slog.Default(), viewer.Type(99))
	})
}

func TestViewerTokenAuth_WrongStreamKey_401(t *testing.T) {
	// Two streams, different derived keys (because stream IDs differ).
	// A token minted for stream 1 must not authorize requests to stream 2
	// — the KDF binds the stream ID.
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	keys := newKeysMap(t, "1")
	// Build stream 2's entry under the same env secret so the only
	// distinguishing input to the KDF is the streamID.
	pk2, err := viewer.DeriveKey([]byte(testEnvSecret), "2", viewer.TypePlaylist)
	require.NoError(t, err)
	sk2, err := viewer.DeriveKey([]byte(testEnvSecret), "2", viewer.TypeSegment)
	require.NoError(t, err)
	keys["2"] = config.ViewerKeys{Playlist: pk2, Segment: sk2}

	tok, err := viewer.Mint(keys["1"].Playlist, clk.Now().Add(time.Hour).UnixMilli())
	require.NoError(t, err)

	r := newTestRouter(clk, keys)
	req := httptest.NewRequest(http.MethodGet, "/stream/2/thing?vt="+tok, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
