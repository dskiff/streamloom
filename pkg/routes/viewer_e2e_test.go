package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/dskiff/streamloom/pkg/config"
	"github.com/dskiff/streamloom/pkg/viewer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vtQueryRe extracts a ?vt=<token> query parameter value from a playlist
// body. Viewer tokens are base64url so the char class matches exactly what
// viewer.Mint emits.
var vtQueryRe = regexp.MustCompile(`\?vt=([A-Za-z0-9_-]+)`)

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

	// 5. GET master playlist with vt → media.m3u8 URI must carry the
	//    viewer's own long-lived vt (master is not cached/rewritten; the
	//    viewer fetches media.m3u8 using the same token they used here).
	req := httptest.NewRequest(http.MethodGet, "/stream/1/stream.m3u8?vt="+vt, nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "media.m3u8?vt="+vt)

	// 6. GET media playlist with the viewer's vt → every emitted URI must
	//    carry a ?vt=<token>. The token is the playlist-scoped short-lived
	//    token baked by the renderer (NOT the viewer's long-lived vt); it
	//    must still verify against the same per-stream key.
	req = httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8?vt="+vt, nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	mediaBody := rec.Body.String()
	// Any stale placeholder system would leak literal "{VT}"; assert absence.
	assert.NotContains(t, mediaBody, "{VT}")

	matches := vtQueryRe.FindAllStringSubmatch(mediaBody, -1)
	require.NotEmpty(t, matches, "media playlist must embed ?vt= on URIs")
	assert.Contains(t, mediaBody, `#EXT-X-MAP:URI="init.mp4?vt=`)
	assert.Contains(t, mediaBody, "segment_0.m4s?vt=")

	// Every embedded token must verify against the segment-derived key (the
	// renderer bakes segment-class tokens) and must NOT verify against the
	// playlist-derived key — that mismatch is what scopes them off
	// playlist routes.
	keys := testViewerKeys(t, "1")
	for _, m := range matches {
		assert.NoError(t, viewer.Verify(keys.Segment, clk.Now(), m[1]),
			"baked token must verify under the stream's segment-derived key")
		assert.ErrorIs(t,
			viewer.Verify(keys.Playlist, clk.Now(), m[1]),
			viewer.ErrBadMAC,
			"baked token must NOT verify under the playlist-derived key")
	}
	initTokMatch := regexp.MustCompile(`init\.mp4\?vt=([A-Za-z0-9_-]+)`).FindStringSubmatch(mediaBody)
	segTokMatch := regexp.MustCompile(`segment_0\.m4s\?vt=([A-Za-z0-9_-]+)`).FindStringSubmatch(mediaBody)
	require.Len(t, initTokMatch, 2)
	require.Len(t, segTokMatch, 2)
	playlistVT := segTokMatch[1]

	// The baked segment-class token must be REFUSED on playlist routes.
	// This is the central defense against the infinite-rotation attack:
	// a scraper who pulls media.m3u8 once and harvests the baked token
	// must not be able to refetch media.m3u8 with it.
	for _, p := range []string{"/stream/1/media.m3u8", "/stream/1/stream.m3u8"} {
		req = httptest.NewRequest(http.MethodGet, p+"?vt="+playlistVT, nil)
		rec = httptest.NewRecorder()
		streamRouter.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code,
			"baked segment-class token must be refused on %s", p)
	}

	// 7. GET init.mp4 with the playlist-scoped vt → 200.
	req = httptest.NewRequest(http.MethodGet, "/stream/1/init.mp4?vt="+playlistVT, nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []byte("init-data"), rec.Body.Bytes())

	// 8. GET segment with the playlist-scoped vt → 200.
	req = httptest.NewRequest(http.MethodGet, "/stream/1/segment_0.m4s?vt="+playlistVT, nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, segData, rec.Body.Bytes())

	// The viewer's original long-lived vt must also still work on any
	// stream route — tokens are not route-scoped.
	req = httptest.NewRequest(http.MethodGet, "/stream/1/segment_0.m4s?vt="+vt, nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

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

// TestE2E_ViewerToken_SegmentURIsStableAcrossRenders is the regression
// test for the HLS URI-stability bug: a segment's ?vt= token must not
// change across successive media-playlist renders. Without this, HLS
// clients that dedupe segments by URI (or that re-fetch EXT-X-MAP when
// its URI changes) churn through every reload.
//
// The test commits a segment, renders, commits a second segment (forcing
// a re-render), renders again, and asserts the first segment's full URI
// line (including ?vt=) is byte-identical across both bodies.
func TestE2E_ViewerToken_SegmentURIsStableAcrossRenders(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	streamRouter, apiRouter, store, _ := testBothRoutersWithViewerKey(t, clk)

	hdrs := initHeaders()
	rec := postInit(apiRouter, "1", "test-token", hdrs, []byte("init-data"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	clk.Set(time.UnixMilli(10000))
	rec = postSegment(apiRouter, "1", "test-token", "0", "5000", "2000", []byte("seg0"))
	require.Equal(t, http.StatusCreated, rec.Code)

	// Mint a long-lived viewer token so we can fetch the playlist through
	// the auth'd route across the 1-hour rollover tested below.
	exp := clk.Now().Add(3 * time.Hour).UnixMilli()
	body, _ := json.Marshal(map[string]any{"expires_at_ms": exp})
	rec = postViewerToken(apiRouter, "1", "test-token", body)
	require.Equal(t, http.StatusCreated, rec.Code)
	var mintResp struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &mintResp))
	vt := mintResp.Token

	s := store.Get("1")
	require.NotNil(t, s)
	require.Eventually(t, func() bool {
		return strings.Contains(s.CachedPlaylist(), "segment_0.m4s")
	}, 2*time.Second, 10*time.Millisecond)

	// First render.
	req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8?vt="+vt, nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	firstBody := rec.Body.String()

	// Commit a second segment, then advance the clock past its timestamp
	// so the renderer makes it eligible. Post first + advance second
	// because a segment's timestamp must be >= the store clock at commit
	// time (after the first-segment exemption is spent).
	rec = postSegment(apiRouter, "1", "test-token", "1", "10000", "2000", []byte("seg1"))
	require.Equal(t, http.StatusCreated, rec.Code)
	clk.Set(time.UnixMilli(13000))
	require.Eventually(t, func() bool {
		return strings.Contains(s.CachedPlaylist(), "segment_1.m4s")
	}, 2*time.Second, 10*time.Millisecond)

	// Second render.
	req = httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8?vt="+vt, nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	secondBody := rec.Body.String()

	// The first segment's URI line (including its ?vt= token) must be
	// byte-identical between the two playlist bodies. This is what HLS
	// clients need to avoid re-downloading segments they already have.
	seg0Re := regexp.MustCompile(`segment_0\.m4s\?vt=[A-Za-z0-9_-]+`)
	firstSeg0 := seg0Re.FindString(firstBody)
	secondSeg0 := seg0Re.FindString(secondBody)
	require.NotEmpty(t, firstSeg0)
	require.NotEmpty(t, secondSeg0)
	assert.Equal(t, firstSeg0, secondSeg0,
		"segment_0 URI must be byte-identical across renders (HLS URI-stability)")

	// Same contract for the init URI across renders within the same hour.
	initRe := regexp.MustCompile(`init\.mp4\?vt=[A-Za-z0-9_-]+`)
	withinHourInit := initRe.FindString(firstBody)
	assert.Equal(t,
		withinHourInit,
		initRe.FindString(secondBody),
		"init URI must be stable within the hour bucket")

	// Cross an hour boundary and commit a third segment to trigger a
	// re-render. The init URI MUST rotate (hour-bucketed anchor) while
	// older segment URIs that remain in the window stay byte-identical.
	clk.Set(time.UnixMilli(int64(time.Hour/time.Millisecond) + 2000))
	rec = postSegment(apiRouter, "1", "test-token",
		"2", strconv.FormatInt(clk.Now().UnixMilli(), 10), "2000", []byte("seg2"))
	require.Equal(t, http.StatusCreated, rec.Code)
	clk.Set(time.UnixMilli(int64(time.Hour/time.Millisecond) + 5000))
	require.Eventually(t, func() bool {
		return strings.Contains(s.CachedPlaylist(), "segment_2.m4s")
	}, 2*time.Second, 10*time.Millisecond)

	req = httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8?vt="+vt, nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	thirdBody := rec.Body.String()

	assert.NotEqual(t, withinHourInit, initRe.FindString(thirdBody),
		"init URI must rotate at the hour bucket boundary")
	// If seg0 is still in the window its URI must be byte-identical to
	// the first render's — segment tokens are anchored to segment
	// timestamp, not wall clock, so an hour rollover must not affect
	// them. (seg0 may have aged out of the window depending on eviction;
	// the assertion is conditional.)
	if strings.Contains(thirdBody, "segment_0.m4s") {
		assert.Equal(t, seg0Re.FindString(firstBody), seg0Re.FindString(thirdBody),
			"segment_0 URI must survive unchanged across an hour rollover")
	}
}

// TestE2E_ViewerToken_KeyRotationInvalidatesOutstanding asserts that
// rotating a stream's viewer-token key (as an operator would via an env
// change + restart, simulated here by mutating the shared map) immediately
// invalidates every token minted under the previous key. This locks in the
// operational "rotate to revoke" contract documented in the README.
func TestE2E_ViewerToken_KeyRotationInvalidatesOutstanding(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	// Build routers from a shared env so mutations to the viewer-key map
	// propagate to both routers — mirroring a live restart where the new
	// process picks up the rotated key.
	streamRouter, apiRouter, store, env := testBothRoutersWithMutableViewerKey(t, clk)

	hdrs := initHeaders()
	rec := postInit(apiRouter, "1", "test-token", hdrs, []byte("init-data"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	clk.Set(time.UnixMilli(10000))
	rec = postSegment(apiRouter, "1", "test-token", "0", "5000", "2000", []byte("seg"))
	require.Equal(t, http.StatusCreated, rec.Code)

	// Mint a token under the original key.
	exp := clk.Now().Add(time.Hour).UnixMilli()
	body, _ := json.Marshal(map[string]any{"expires_at_ms": exp})
	rec = postViewerToken(apiRouter, "1", "test-token", body)
	require.Equal(t, http.StatusCreated, rec.Code)
	var mintResp struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &mintResp))
	oldTok := mintResp.Token

	// Sanity check: token is accepted pre-rotation.
	req := httptest.NewRequest(http.MethodGet, "/stream/1/segment_0.m4s?vt="+oldTok, nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// Rotate the key by replacing stream 1's derived-key entry with one
	// produced from a completely different env secret — this mirrors an
	// operator-driven restart under a new SL_STREAM_1_VIEWER_TOKEN_KEY.
	newRawSecret := strings.Repeat("z", 32)
	newPlaylist, err := viewer.DeriveKey([]byte(newRawSecret), "1", viewer.TypePlaylist)
	require.NoError(t, err)
	newSegment, err := viewer.DeriveKey([]byte(newRawSecret), "1", viewer.TypeSegment)
	require.NoError(t, err)
	env.STREAM_VIEWER_TOKEN_KEYS["1"] = config.ViewerKeys{
		Playlist: newPlaylist,
		Segment:  newSegment,
	}

	// Old token must now fail to verify under the new key. Every stream
	// route is covered to guard against per-route-group key staleness.
	for _, p := range []string{
		"/stream/1/stream.m3u8",
		"/stream/1/media.m3u8",
		"/stream/1/init.mp4",
		"/stream/1/segment_0.m4s",
	} {
		req = httptest.NewRequest(http.MethodGet, p+"?vt="+oldTok, nil)
		rec = httptest.NewRecorder()
		streamRouter.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code,
			"post-rotation, old token must be refused on %s", p)
	}

	// A token minted under the new key works immediately. Uses the same
	// clock so the 5-minute TTL floor is satisfied; a short-TTL happy-path
	// mint is the simplest way to prove the new key is live across both
	// routers.
	newExp := clk.Now().Add(time.Hour).UnixMilli()
	newBody, _ := json.Marshal(map[string]any{"expires_at_ms": newExp})
	rec = postViewerToken(apiRouter, "1", "test-token", newBody)
	require.Equal(t, http.StatusCreated, rec.Code)
	var newResp struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &newResp))

	req = httptest.NewRequest(http.MethodGet, "/stream/1/segment_0.m4s?vt="+newResp.Token, nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "token minted under rotated key must work")
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

	// Mint a short-but-valid token (10 minutes, comfortably above the
	// 5-minute floor even after minute alignment).
	exp := clk.Now().Add(10 * time.Minute).UnixMilli()
	body, _ := json.Marshal(map[string]any{"expires_at_ms": exp})
	rec = postViewerToken(apiRouter, "1", "test-token", body)
	require.Equal(t, http.StatusCreated, rec.Code)

	var mintResp struct {
		Token       string `json:"token"`
		ExpiresAtMs int64  `json:"expires_at_ms"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &mintResp))
	vt := mintResp.Token

	// Advance the clock well past the encoded expiry.
	clk.Set(time.UnixMilli(mintResp.ExpiresAtMs + 1000))

	req := httptest.NewRequest(http.MethodGet, "/stream/1/segment_0.m4s?vt="+vt, nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
