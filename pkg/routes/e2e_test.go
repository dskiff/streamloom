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
	// no-store on master: each session needs its own master fetch to
	// capture a fresh wall-clock-aligned `?to=`. A CDN caching master
	// would lock multiple sessions to one offset.
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.NotEmpty(t, rec.Header().Get("Content-Length"))

	body := rec.Body.String()
	assert.Contains(t, body, "#EXTM3U")
	assert.Contains(t, body, "#EXT-X-VERSION:7")
	assert.Contains(t, body, "BANDWIDTH=4000000")
	assert.Contains(t, body, "RESOLUTION=1920x1080")
	assert.Contains(t, body, `CODECS="avc1.64001f"`)
	assert.Contains(t, body, "FRAME-RATE=23.976")
	// Pre-live-edge (no segments yet): master emits a bare `media.m3u8`
	// URI; once segments arrive subsequent master fetches will bake the
	// fresh `?to=` (covered by TestMasterPlaylist_BakesStickyOffset).
	assert.Contains(t, body, "\nmedia.m3u8\n")
}

// TestMasterPlaylist_BakesStickyOffset asserts the master playlist
// bakes a fresh `?to=<magnitude>` into the media URI once a snapshot
// is available. This is what gives the iOS-stable property: the
// player parses the URL once and reloads it every cycle, so the
// rendered EXT-X-START stays byte-identical for the session.
func TestMasterPlaylist_BakesStickyOffset(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	streamRouter, apiRouter, store, _ := testBothRoutersWithToken(t, clk)

	hdrs := initHeaders()
	hdrs["X-SL-MAX-LOOKAHEAD-MS"] = "10000"
	rec := postInit(apiRouter, "1", "test-token", hdrs, []byte("init-data"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	rec = postSegment(apiRouter, "1", "test-token", "0", "8000", "2000", []byte("seg"))
	require.Equal(t, http.StatusCreated, rec.Code)

	s := store.Get("1")
	require.NotNil(t, s)
	require.Eventually(t, func() bool {
		return strings.Contains(s.CachedPlaylist(), "segment_0.m4s")
	}, 2*time.Second, 10*time.Millisecond)

	// At clock=1500: tail=10000, gap=(10000-1500)/1000=8.500.
	clk.Set(time.UnixMilli(1500))
	req := httptest.NewRequest(http.MethodGet, "/stream/1/stream.m3u8", nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "\nmedia.m3u8?to=8.500\n",
		"master must bake the fresh tail-to-now offset into the media URI")
}

// TestMasterPlaylist_BakesStickyOffsetClampsToFloor mirrors the
// sticky-floor invariant at the master level: when the tail sits at
// or behind wall clock the baked magnitude clamps to MinHoldBackSecs
// rather than going zero / positive.
func TestMasterPlaylist_BakesStickyOffsetClampsToFloor(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	streamRouter, apiRouter, store, _ := testBothRoutersWithToken(t, clk)

	hdrs := initHeaders()
	hdrs["X-SL-MAX-LOOKAHEAD-MS"] = "10000" // MinHoldBack=6 (3 × target=2)
	rec := postInit(apiRouter, "1", "test-token", hdrs, []byte("init-data"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	rec = postSegment(apiRouter, "1", "test-token", "0", "2000", "2000", []byte("seg")) // endMs=4000
	require.Equal(t, http.StatusCreated, rec.Code)

	s := store.Get("1")
	require.NotNil(t, s)
	require.Eventually(t, func() bool {
		return strings.Contains(s.CachedPlaylist(), "segment_0.m4s")
	}, 2*time.Second, 10*time.Millisecond)

	// 100s past endMs — raw gap = -100s → clamp to MinHoldBack=6.
	clk.Set(time.UnixMilli(104_000))
	req := httptest.NewRequest(http.MethodGet, "/stream/1/stream.m3u8", nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "\nmedia.m3u8?to=6.000\n")
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

	rec, _ = fetchMediaViaMaster(t, streamRouter, "/stream/1/stream.m3u8")
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

	rec, _ = fetchMediaViaMaster(t, streamRouter, "/stream/1/stream.m3u8")
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()

	// HOLD-BACK reflects the configured look-ahead cap (6000ms = 6.000s).
	assert.Contains(t, body, "#EXT-X-SERVER-CONTROL:HOLD-BACK=6.000\n")

	// The master baked a fresh offset against the current snapshot:
	// tail PDT (8.000s) − wall clock (1.000s) = 7.000s. The sticky
	// URL the master emits carries this magnitude as `?to=7.000` and
	// the media handler renders it verbatim into EXT-X-START.
	// PRECISE=YES eliminates segment-boundary snap. Start content
	// PDT = 8.000 − 7.000 = 1.000s, exactly wall clock at session
	// start.
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

// TestE2E_StartOffsetTracksWallClock exercises cross-device sync
// end-to-end through the master-bake flow: two viewers fetch
// stream.m3u8 at different walls, each one's master bakes a different
// `?to=` magnitude into the media URI, and the rendered playlist each
// fetches via that URI places each viewer's start content PDT at
// their own wall clock. The sticky URL is reused on every reload, so
// EXT-X-START stays byte-identical within each session — the iOS
// compat property.
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

	// Viewer A: master fetch at wall=1000 bakes ?to=9.000 (tail-to-now
	// gap = (10000-1000)/1000). Following the URL renders EXT-X-START
	// with that magnitude verbatim.
	clk.Set(time.UnixMilli(1000))
	recA, urlA := fetchMediaViaMaster(t, streamRouter, "/stream/1/stream.m3u8")
	require.Equal(t, http.StatusOK, recA.Code)
	assert.Contains(t, urlA, "to=9.000", "A's sticky URL must bake 9.000s")
	offA := extractStartOffsetSecs(t, recA.Body.String())
	assert.InDelta(t, 9.0, offA, 0.001)

	// Viewer B: master fetch at wall=2200 (same cached body, new
	// session) bakes ?to=7.800.
	clk.Set(time.UnixMilli(2200))
	recB, urlB := fetchMediaViaMaster(t, streamRouter, "/stream/1/stream.m3u8")
	require.Equal(t, http.StatusOK, recB.Code)
	assert.Contains(t, urlB, "to=7.800", "B's sticky URL must bake 7.800s")
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

	// Sticky invariant: A reloading their own sticky URL at a much
	// later wall clock still gets a byte-identical EXT-X-START. This
	// is what current iOS/macOS clients require — the player parses
	// the URL once from the master and reloads it every cycle.
	clk.Set(time.UnixMilli(8000))
	reqAReload := httptest.NewRequest(http.MethodGet, urlA, nil)
	recAReload := httptest.NewRecorder()
	streamRouter.ServeHTTP(recAReload, reqAReload)
	require.Equal(t, http.StatusOK, recAReload.Code)
	assert.Equal(t, extractStartOffsetSecs(t, recA.Body.String()),
		extractStartOffsetSecs(t, recAReload.Body.String()),
		"reload of A's sticky URL must echo the same EXT-X-START across walls")
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

	rec, _ = fetchMediaViaMaster(t, streamRouter, "/stream/1/stream.m3u8")
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

	rec, _ = fetchMediaViaMaster(t, streamRouter, "/stream/myStream/stream.m3u8")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "segment_0.m4s")
}
