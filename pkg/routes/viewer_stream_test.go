package routes

import (
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

// mintVT produces a viewer token valid for one hour from clk.
func mintVT(t *testing.T, clk clock.Clock, key []byte) string {
	t.Helper()
	tok, err := viewer.Mint(key, clk.Now().Add(time.Hour).UnixMilli())
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

	expired, err := viewer.Mint(testViewerKey, clk.Now().Add(-time.Second).UnixMilli())
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
	bad := vt[:len(vt)-1] + "A"
	if bad == vt {
		bad = vt[:len(vt)-1] + "B"
	}

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
