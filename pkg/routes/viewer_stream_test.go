package routes

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/dskiff/streamloom/pkg/viewer"
	"github.com/dskiff/streamloom/pkg/watcher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mintVT produces a TypeViewer token valid for one hour from clk.
func mintVT(t *testing.T, clk clock.Clock, key []byte) string {
	t.Helper()
	tok, err := viewer.Mint(key, clk.Now().Add(time.Hour).UnixMilli(), viewer.TypeViewer)
	require.NoError(t, err)
	return tok
}

// mintSegmentVT produces a TypeSegment token valid for one hour from clk.
// Used to assert type-scoping: TypeSegment must be refused on playlist routes.
func mintSegmentVT(t *testing.T, clk clock.Clock, key []byte) string {
	t.Helper()
	tok, err := viewer.Mint(key, clk.Now().Add(time.Hour).UnixMilli(), viewer.TypeSegment)
	require.NoError(t, err)
	return tok
}

func TestStream_NoViewerKey_PublicPlayback(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	router, store, _ := testStreamRouter(t, clk)
	initStream(t, store, "1")
	s := store.Get("1")
	require.NotNil(t, s)
	commitSegment(t, s, 1, []byte("seg-data"), clk.Now().UnixMilli())

	req := httptest.NewRequest(http.MethodGet, "/stream/1/segment_1.m4s", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestStream_ViewerKey_MissingVT_401(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	router, store, _ := testStreamRouterWithViewerKey(t, clk)
	initStream(t, store, "1")
	s := store.Get("1")
	require.NotNil(t, s)
	commitSegment(t, s, 1, []byte("seg-data"), clk.Now().UnixMilli())

	req := httptest.NewRequest(http.MethodGet, "/stream/1/segment_1.m4s", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestStream_ViewerKey_ValidVT_200(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	router, store, _ := testStreamRouterWithViewerKey(t, clk)
	initStream(t, store, "1")
	s := store.Get("1")
	require.NotNil(t, s)
	commitSegment(t, s, 1, []byte("seg-data"), clk.Now().UnixMilli())

	vt := mintVT(t, clk, testViewerKey)
	req := httptest.NewRequest(http.MethodGet, "/stream/1/segment_1.m4s?vt="+vt, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestStream_ViewerKey_ExpiredVT_401(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	router, store, _ := testStreamRouterWithViewerKey(t, clk)
	initStream(t, store, "1")
	s := store.Get("1")
	require.NotNil(t, s)
	commitSegment(t, s, 1, []byte("seg-data"), clk.Now().UnixMilli())

	expired, err := viewer.Mint(testViewerKey, clk.Now().Add(-time.Second).UnixMilli(), viewer.TypeViewer)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/stream/1/segment_1.m4s?vt="+expired, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestStream_ViewerKey_TamperedVT_401(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	router, store, _ := testStreamRouterWithViewerKey(t, clk)
	initStream(t, store, "1")
	s := store.Get("1")
	require.NotNil(t, s)
	commitSegment(t, s, 1, []byte("seg-data"), clk.Now().UnixMilli())

	vt := mintVT(t, clk, testViewerKey)
	// Tamper at the byte level (not the base64 character level): the
	// 22-byte payload encodes to 30 base64url characters, 4 of which are
	// excess padding bits, so flipping a single trailing character can
	// flip only unused bits and leave the decoded payload unchanged.
	raw, err := base64.RawURLEncoding.DecodeString(vt)
	require.NoError(t, err)
	raw[len(raw)-1] ^= 0x01
	bad := base64.RawURLEncoding.EncodeToString(raw)

	req := httptest.NewRequest(http.MethodGet, "/stream/1/segment_1.m4s?vt="+bad, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// 401 on a stream route must NOT record a viewer. The ViewerTokenAuth
// middleware is placed before RecordWatcher specifically to guarantee this.
func TestStream_UnauthorizedRequest_DoesNotRecordWatcher(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	router, store, tracker := testStreamRouterWithViewerKey(t, clk)
	initStream(t, store, "1")

	req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	req.RemoteAddr = "203.0.113.7:1234"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	count := tracker.ActiveCount("1", watcher.DefaultWindowMs)
	assert.Equal(t, 0, count, "401 requests must not inflate watcher count")
}

// TestStream_PlaylistRoutes_RejectSegmentType asserts that a token minted
// with TypeSegment (the class the renderer bakes into init/segment URIs) is
// refused on playlist routes. Without this check, a holder of a baked
// playlist token could refetch media.m3u8 and rotate their token forever.
func TestStream_PlaylistRoutes_RejectSegmentType(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	router, store, _ := testStreamRouterWithViewerKey(t, clk)
	initStream(t, store, "1")
	s := store.Get("1")
	require.NotNil(t, s)
	commitSegment(t, s, 1, []byte("seg-data"), clk.Now().UnixMilli())

	segVT := mintSegmentVT(t, clk, testViewerKey)
	for _, path := range []string{"/stream/1/media.m3u8", "/stream/1/stream.m3u8"} {
		req := httptest.NewRequest(http.MethodGet, path+"?vt="+segVT, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code,
			"segment-typed token must be refused on %s", path)
	}
}

// TestStream_SegmentRoutes_AcceptBothTypes asserts init/segment routes accept
// both a direct TypeViewer grant and the TypeSegment token the renderer
// bakes into playlist URIs.
func TestStream_SegmentRoutes_AcceptBothTypes(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	router, store, _ := testStreamRouterWithViewerKey(t, clk)
	initStream(t, store, "1")
	s := store.Get("1")
	require.NotNil(t, s)
	commitSegment(t, s, 1, []byte("seg-data"), clk.Now().UnixMilli())

	for _, tok := range []string{
		mintVT(t, clk, testViewerKey),
		mintSegmentVT(t, clk, testViewerKey),
	} {
		req := httptest.NewRequest(http.MethodGet, "/stream/1/segment_1.m4s?vt="+tok, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)

		req = httptest.NewRequest(http.MethodGet, "/stream/1/init.mp4?vt="+tok, nil)
		rec = httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	}
}

func TestStream_AuthorizedRequest_DoesRecordWatcher(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	router, store, tracker := testStreamRouterWithViewerKey(t, clk)
	initStream(t, store, "1")
	s := store.Get("1")
	require.NotNil(t, s)
	commitSegment(t, s, 1, []byte("seg-data"), clk.Now().UnixMilli())

	vt := mintVT(t, clk, testViewerKey)
	req := httptest.NewRequest(http.MethodGet, "/stream/1/segment_1.m4s?vt="+vt, nil)
	req.RemoteAddr = "203.0.113.7:1234"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	count := tracker.ActiveCount("1", watcher.DefaultWindowMs)
	assert.Equal(t, 1, count)
}
