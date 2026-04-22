package routes

import (
	"crypto/sha256"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
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

	// 4. Retrieve init.mp4 via stream server.
	s := store.Get("1")
	require.NotNil(t, s)
	req := httptest.NewRequest(http.MethodGet, "/stream/1/init.mp4", nil)
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

// TestE2E_LookaheadLiveEdge pushes segments with PDTs spanning several
// target durations ahead of wall clock and confirms: (1) the playlist
// tail sits approximately at now + lookahead rather than at wall clock,
// (2) segments beyond the cap are excluded until they cross it, and
// (3) the HOLD-BACK header matches the configured cap. The contiguity
// gate is covered separately by TestE2E_LookaheadContiguityUnderReordering.
func TestE2E_LookaheadLiveEdge(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	streamRouter, apiRouter, store, _ := testBothRoutersWithToken(t, clk)

	hdrs := initHeaders() // target-duration=2 → default lookahead=6000ms
	rec := postInit(apiRouter, "1", "test-token", hdrs, []byte("init-data"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	s := store.Get("1")
	require.NotNil(t, s)
	require.Equal(t, int64(6000), s.MaxLookaheadMs())

	// Clock at 1000ms; push indices 0..4 at ts=2000,4000,6000,8000,10000.
	// With lookahead=6000, cap at now=1000 is 7000 → indices 0,1,2 are in,
	// indices 3,4 are past the cap.
	clk.Set(time.UnixMilli(1000))
	for i, ts := range []int64{2000, 4000, 6000, 8000, 10000} {
		rec := postSegment(apiRouter, "1", "test-token",
			strconv.Itoa(i), strconv.FormatInt(ts, 10), "2000",
			[]byte("seg"))
		require.Equal(t, http.StatusCreated, rec.Code)
	}

	require.Eventually(t, func() bool {
		p := s.CachedPlaylist()
		return p != "" && strings.Contains(p, "segment_2.m4s")
	}, 2*time.Second, 10*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()

	// HOLD-BACK reflects the configured look-ahead cap (6000ms = 6.000s).
	assert.Contains(t, body, "#EXT-X-SERVER-CONTROL:HOLD-BACK=6.000\n")

	// EXT-X-START places the player at wall-clock now inside the
	// playlist: TIME-OFFSET magnitude = tail PDT (8.000s) − wall clock
	// (1.000s) = 7.000s. PRECISE=YES eliminates segment-boundary snap.
	// Start content PDT = 8.000 − 7.000 = 1.000s, exactly wall clock.
	assert.Contains(t, body, "#EXT-X-START:TIME-OFFSET=-7.000,PRECISE=YES\n")

	// Tail PDT ≈ 1970-01-01T00:00:06.000Z (now + 6s).
	assert.Contains(t, body, "#EXT-X-PROGRAM-DATE-TIME:1970-01-01T00:00:06.000Z")

	// Indices within the cap are present; beyond are excluded.
	assert.Contains(t, body, "segment_0.m4s")
	assert.Contains(t, body, "segment_1.m4s")
	assert.Contains(t, body, "segment_2.m4s")
	assert.NotContains(t, body, "segment_3.m4s",
		"segment at ts=8000 is past now+lookahead=7000 and must not appear")
	assert.NotContains(t, body, "segment_4.m4s")
}

// TestE2E_StartOffsetTracksWallClock exercises the dynamic TIME-OFFSET
// end-to-end: push a segment whose tail PDT sits well ahead of wall
// clock, then fetch the playlist at two different clocks on the same
// cached body. Each response's TIME-OFFSET must equal the tail-to-now
// gap, so each viewer's start content PDT resolves to their own wall
// clock — the cross-device-sync guarantee.
func TestE2E_StartOffsetTracksWallClock(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	streamRouter, apiRouter, store, _ := testBothRoutersWithToken(t, clk)

	// Target-duration=2s, look-ahead=10s → MinHoldBack=6. The tail
	// needs to be far enough ahead of both fetch clocks to beat the
	// floor.
	hdrs := initHeaders()
	hdrs["X-SL-MAX-LOOKAHEAD-MS"] = "10000"
	rec := postInit(apiRouter, "1", "test-token", hdrs, []byte("init-data"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	s := store.Get("1")
	require.NotNil(t, s)

	// Push one segment ending at PDT=10000. Render clock at 0 keeps
	// ts=8000 within the lookahead cap (cutoff=10000).
	rec = postSegment(apiRouter, "1", "test-token", "0", "8000", "2000", []byte("seg"))
	require.Equal(t, http.StatusCreated, rec.Code)

	require.Eventually(t, func() bool {
		return strings.Contains(s.CachedPlaylist(), "segment_0.m4s")
	}, 2*time.Second, 10*time.Millisecond)

	const endMs = 10000

	// Viewer A at clock=1000: gap = (10000−1000)/1000 = 9.000s.
	clk.Set(time.UnixMilli(1000))
	reqA := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	recA := httptest.NewRecorder()
	streamRouter.ServeHTTP(recA, reqA)
	require.Equal(t, http.StatusOK, recA.Code)
	offA := extractStartOffsetSecs(t, recA.Body.String())
	assert.InDelta(t, 9.0, offA, 0.001)

	// Viewer B at clock=2200 (same cached body): gap = 7.800s.
	clk.Set(time.UnixMilli(2200))
	reqB := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	recB := httptest.NewRecorder()
	streamRouter.ServeHTTP(recB, reqB)
	require.Equal(t, http.StatusOK, recB.Code)
	offB := extractStartOffsetSecs(t, recB.Body.String())
	assert.InDelta(t, 7.8, offB, 0.001)

	// Each viewer's start content PDT equals their own wall clock, so
	// the two start PDTs differ by exactly their wall-clock gap.
	startA := int64(endMs) - int64(offA*1000)
	startB := int64(endMs) - int64(offB*1000)
	assert.InDelta(t, 1000, startA, 1, "viewer A start PDT must match wallA=1000")
	assert.InDelta(t, 2200, startB, 1, "viewer B start PDT must match wallB=2200")
	assert.InDelta(t, 1200, startB-startA, 1,
		"staggered viewers diverge in content start by exactly their wall-clock gap; got %d", startB-startA)
}

func TestE2E_LookaheadContiguityUnderReordering(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	streamRouter, apiRouter, store, _ := testBothRoutersWithToken(t, clk)

	hdrs := initHeaders()
	rec := postInit(apiRouter, "1", "test-token", hdrs, []byte("init-data"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	s := store.Get("1")
	require.NotNil(t, s)

	// Push 0, 1, 2 then leapfrog to 4 — transcoder delivered index 4 before 3.
	// All within the default 6000ms look-ahead at clock=1000 (cap=7000).
	clk.Set(time.UnixMilli(1000))
	for _, c := range []struct {
		idx string
		ts  string
	}{
		{"0", "2000"},
		{"1", "4000"},
		{"2", "6000"},
	} {
		rec := postSegment(apiRouter, "1", "test-token", c.idx, c.ts, "2000", []byte("seg"))
		require.Equal(t, http.StatusCreated, rec.Code)
	}
	// Advance clock so index 4's timestamp falls within the cap once committed.
	// At clock=4000 the cap is 10000, so ts=10000 sits at the boundary.
	clk.Set(time.UnixMilli(4000))
	rec = postSegment(apiRouter, "1", "test-token", "4", "10000", "2000", []byte("seg"))
	require.Equal(t, http.StatusCreated, rec.Code)

	// Contiguity gate must hold the tail at index 2 because 3 is missing.
	require.Eventually(t, func() bool {
		return strings.Contains(s.CachedPlaylist(), "segment_2.m4s")
	}, 2*time.Second, 10*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "segment_2.m4s")
	assert.NotContains(t, body, "segment_4.m4s",
		"contiguity gate must truncate before the gap at index 3")

	// Fill the gap: index 3 arrives. Now the playlist extends to 4.
	rec = postSegment(apiRouter, "1", "test-token", "3", "8000", "2000", []byte("seg"))
	require.Equal(t, http.StatusCreated, rec.Code)

	require.Eventually(t, func() bool {
		return strings.Contains(s.CachedPlaylist(), "segment_4.m4s")
	}, 2*time.Second, 10*time.Millisecond)
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
	req := httptest.NewRequest(http.MethodGet, "/stream/myStream/init.mp4", nil)
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
