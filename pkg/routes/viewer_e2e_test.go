package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2E_ViewerToken_FullFlow exercises the complete viewer-token flow:
// mint via API → GET master playlist → GET media playlist → GET init.mp4 →
// GET segment, asserting the token is propagated through every emitted URI
// and that requests without a token are rejected.
func TestE2E_ViewerToken_FullFlow(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	streamRouter, apiRouter, store, _ := testBothRoutersWithViewerKey(t, clk)

	// 1. Init the stream.
	hdrs := initHeaders()
	rec := postInit(apiRouter, "1", "test-token", hdrs, []byte("init-data"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	// 2. Advance time so segments are eligible for the playlist.
	clk.Set(time.UnixMilli(10000))

	// 3. Push a segment.
	segData := []byte("hello-segment")
	rec = postSegment(apiRouter, "1", "test-token", "0", "5000", "2000", segData)
	require.Equal(t, http.StatusCreated, rec.Code)

	// 4. Mint a viewer token via the API.
	exp := clk.Now().Add(time.Hour).UnixMilli()
	body, _ := json.Marshal(map[string]any{"expires_at_ms": exp})
	rec = postViewerToken(apiRouter, "1", "test-token", body)
	require.Equal(t, http.StatusCreated, rec.Code)

	var mintResp struct {
		Token       string `json:"token"`
		ExpiresAtMs int64  `json:"expires_at_ms"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &mintResp))
	require.NotEmpty(t, mintResp.Token)
	vt := mintResp.Token

	// Wait for the media playlist to include the committed segment.
	s := store.Get("1")
	require.NotNil(t, s)
	require.Eventually(t, func() bool {
		p := s.CachedPlaylist()
		return p != "" && strings.Contains(p, "segment_0.m4s")
	}, 2*time.Second, 10*time.Millisecond)

	// 5. GET master playlist with vt → media.m3u8 URI must carry ?vt=<token>.
	req := httptest.NewRequest(http.MethodGet, "/stream/1/stream.m3u8?vt="+vt, nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "media.m3u8?vt="+vt)

	// 6. GET media playlist with vt → every emitted URI must carry ?vt=<token>.
	req = httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8?vt="+vt, nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	mediaBody := rec.Body.String()
	assert.Contains(t, mediaBody, `#EXT-X-MAP:URI="init.mp4?vt=`+vt+`"`)
	assert.Contains(t, mediaBody, "segment_0.m4s?vt="+vt)
	// No stray placeholders should leak through to the client.
	assert.NotContains(t, mediaBody, "{VT}")

	// 7. GET init.mp4 with vt → 200.
	req = httptest.NewRequest(http.MethodGet, "/stream/1/init.mp4?vt="+vt, nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []byte("init-data"), rec.Body.Bytes())

	// 8. GET segment with vt → 200.
	req = httptest.NewRequest(http.MethodGet, "/stream/1/segment_0.m4s?vt="+vt, nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, segData, rec.Body.Bytes())

	// 9. Requests WITHOUT vt on the same resources must be rejected.
	paths := []string{
		"/stream/1/stream.m3u8",
		"/stream/1/media.m3u8",
		"/stream/1/init.mp4",
		"/stream/1/segment_0.m4s",
	}
	for _, p := range paths {
		req = httptest.NewRequest(http.MethodGet, p, nil)
		rec = httptest.NewRecorder()
		streamRouter.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "expected 401 for %s without vt", p)
	}
}

// TestE2E_ViewerToken_ExpiredAfterMint ensures a token that was valid at mint
// time is rejected once the mock clock advances past its expiry.
func TestE2E_ViewerToken_ExpiredAfterMint(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	streamRouter, apiRouter, store, _ := testBothRoutersWithViewerKey(t, clk)

	hdrs := initHeaders()
	rec := postInit(apiRouter, "1", "test-token", hdrs, []byte("init-data"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	clk.Set(time.UnixMilli(10000))
	rec = postSegment(apiRouter, "1", "test-token", "0", "5000", "2000", []byte("seg"))
	require.Equal(t, http.StatusCreated, rec.Code)

	// Mint a token that expires one second after "now".
	exp := clk.Now().Add(time.Second).UnixMilli()
	body, _ := json.Marshal(map[string]any{"expires_at_ms": exp})
	rec = postViewerToken(apiRouter, "1", "test-token", body)
	require.Equal(t, http.StatusCreated, rec.Code)

	var mintResp struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &mintResp))
	vt := mintResp.Token

	// Advance the clock well past expiry.
	clk.Set(time.UnixMilli(exp + 1000))

	req := httptest.NewRequest(http.MethodGet, "/stream/1/segment_0.m4s?vt="+vt, nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
