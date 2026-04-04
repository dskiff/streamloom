package routes

import (
	"bytes"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

	// 4. Retrieve init.mp4 by generation via stream server.
	s := store.Get("1")
	require.NotNil(t, s)
	req := httptest.NewRequest(http.MethodGet, "/stream/1/init_0.mp4", nil)
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

func TestE2E_DiscontinuityFlow(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	streamRouter, apiRouter, store, _ := testBothRoutersWithToken(t, clk)

	// 1. Init stream at generation 0.
	hdrs := initHeaders()
	rec := postInit(apiRouter, "1", "test-token", hdrs, []byte("init-gen0"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	// 2. Push gen=0 segments (future timestamps, clock still at 0).
	rec = postSegment(apiRouter, "1", "test-token", "0", "5000", "2000", []byte("seg0"))
	require.Equal(t, http.StatusCreated, rec.Code)
	rec = postSegment(apiRouter, "1", "test-token", "1", "7000", "2000", []byte("seg1"))
	require.Equal(t, http.StatusCreated, rec.Code)

	// 3. Add init for generation 1 (subsequent init, only needs generation + body).
	rec = postInitForGeneration(apiRouter, "1", "test-token", "1", []byte("init-gen1"))
	require.Equal(t, http.StatusCreated, rec.Code)

	// 4. Push gen=1 segment.
	rec = postSegmentWithGen(apiRouter, "1", "test-token", "2", "9000", "2000", "1", []byte("seg2"))
	require.Equal(t, http.StatusCreated, rec.Code)

	// 5. Advance clock past the last segment's timestamp (9000) so all three
	// are eligible. Push a gen=1 segment at a future timestamp to wake the
	// renderer via notifyCh.
	clk.Set(time.UnixMilli(10000))
	rec = postSegmentWithGen(apiRouter, "1", "test-token", "3", "11000", "2000", "1", []byte("seg3"))
	require.Equal(t, http.StatusCreated, rec.Code)
	// Advance past segment_3's timestamp too, then push another to wake renderer.
	clk.Set(time.UnixMilli(12000))
	rec = postSegmentWithGen(apiRouter, "1", "test-token", "4", "13000", "2000", "1", []byte("seg4"))
	require.Equal(t, http.StatusCreated, rec.Code)

	// 6. Verify init segments are served by generation.
	req := httptest.NewRequest(http.MethodGet, "/stream/1/init_0.mp4", nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []byte("init-gen0"), rec.Body.Bytes())

	req = httptest.NewRequest(http.MethodGet, "/stream/1/init_1.mp4", nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []byte("init-gen1"), rec.Body.Bytes())

	// 7. Verify media playlist contains discontinuity.
	s := store.Get("1")
	require.NotNil(t, s)
	require.Eventually(t, func() bool {
		p := s.CachedPlaylist()
		return p != "" && strings.Contains(p, "segment_3.m4s") &&
			strings.Contains(p, "#EXT-X-DISCONTINUITY\n")
	}, 2*time.Second, 10*time.Millisecond)

	req = httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	rec = httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")

	// Verify the exact line ordering of the playlist.
	// Expected structure:
	//   #EXTM3U
	//   #EXT-X-VERSION:7
	//   #EXT-X-TARGETDURATION:...
	//   #EXT-X-MEDIA-SEQUENCE:0
	//   #EXT-X-DISCONTINUITY-SEQUENCE:0
	//   #EXT-X-MAP:URI="init_0.mp4"       ← gen 0 MAP (first segment)
	//   ...segment_0.m4s...
	//   ...segment_1.m4s...
	//   #EXT-X-DISCONTINUITY                ← generation change
	//   #EXT-X-MAP:URI="init_1.mp4"        ← gen 1 MAP
	//   ...segment_2.m4s...
	//   ...segment_3.m4s...

	// Helper to find the first line containing a substring.
	lineOf := func(substr string) int {
		for i, l := range lines {
			if strings.Contains(l, substr) {
				return i
			}
		}
		t.Fatalf("expected line containing %q in playlist:\n%s", substr, body)
		return -1
	}

	// Helper to find the first line that exactly equals a string.
	exactLineOf := func(target string) int {
		for i, l := range lines {
			if l == target {
				return i
			}
		}
		t.Fatalf("expected exact line %q in playlist:\n%s", target, body)
		return -1
	}

	// Header lines come first in order.
	assert.Equal(t, 0, lineOf("#EXTM3U"), "EXTM3U must be first line")
	assert.Less(t, lineOf("#EXT-X-VERSION:7"), lineOf("#EXT-X-MEDIA-SEQUENCE:0"))
	assert.Less(t, lineOf("#EXT-X-MEDIA-SEQUENCE:0"), lineOf("#EXT-X-DISCONTINUITY-SEQUENCE:0"))

	// Gen 0 MAP comes before gen 0 segments.
	mapGen0 := exactLineOf(`#EXT-X-MAP:URI="init_0.mp4"`)
	seg0 := exactLineOf("segment_0.m4s")
	seg1 := exactLineOf("segment_1.m4s")
	assert.Less(t, mapGen0, seg0, "gen 0 MAP before segment_0")
	assert.Less(t, seg0, seg1, "segment_0 before segment_1")

	// DISCONTINUITY comes after gen 0 segments and before gen 1 MAP.
	discLine := exactLineOf("#EXT-X-DISCONTINUITY")
	mapGen1 := exactLineOf(`#EXT-X-MAP:URI="init_1.mp4"`)
	seg2 := exactLineOf("segment_2.m4s")
	seg3 := exactLineOf("segment_3.m4s")
	assert.Less(t, seg1, discLine, "segment_1 before DISCONTINUITY")
	assert.Equal(t, discLine+1, mapGen1, "MAP for gen 1 immediately after DISCONTINUITY")
	assert.Less(t, mapGen1, seg2, "gen 1 MAP before segment_2")
	assert.Less(t, seg2, seg3, "segment_2 before segment_3 (both gen 1)")

	// Exactly one DISCONTINUITY tag (between gen 0 and gen 1).
	assert.Equal(t, 1, strings.Count(body, "#EXT-X-DISCONTINUITY\n"))

	// Two MAP entries total.
	assert.Equal(t, 2, strings.Count(body, "#EXT-X-MAP:URI="))
}

func TestE2E_DiscontinuitySequenceIncrements(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))

	// Use store.Init directly so we can control backward-buffer-size=1
	// (aggressive eviction) and a small playlist window=3.
	store := stream.NewStore(clk)
	l := slog.Default()
	env := config.Env{
		STREAM_MAX_BUFFER_BYTES: config.DefaultStreamMaxBufferBytes,
		BUFFER_WORKING_SPACE:    config.DefaultBufferWorkingSpace,
		STREAM_TOKENS: map[string]config.TokenDigest{
			"1": sha256.Sum256([]byte("Bearer test-token")),
		},
	}
	tracker := watcher.NewTracker(clk)
	streamRouter := Stream(l, env, store, nil, tracker)

	meta := stream.Metadata{
		Bandwidth:          4000000,
		Codecs:             "avc1.64001f",
		Width:              1920,
		Height:             1080,
		FrameRate:          23.976,
		TargetDurationSecs: 2,
	}
	// backwardBufferSize=1, playlistWindowSize=3
	err := store.Init("1", meta, []byte("init-gen0"), 0, 20, 1024, 1, 2, 3)
	require.NoError(t, err)
	t.Cleanup(func() { store.Delete("1") })

	s := store.Get("1")
	require.NotNil(t, s)

	require.NoError(t, s.AddInitEntry(1, []byte("init-gen1")))

	// Push gen=0 segments at indices 0-2, timestamps 1000,3000,5000.
	for i := uint32(0); i < 3; i++ {
		buf, ok := s.AcquireSlot()
		require.True(t, ok)
		_, _ = buf.ReadFrom(bytes.NewReader([]byte("seg")))
		require.NoError(t, s.CommitSlot(i, buf, int64(1000+i*2000), 2000, 0))
	}

	// Push gen=1 segments at indices 3-5, timestamps 7000,9000,11000.
	for i := uint32(3); i < 6; i++ {
		buf, ok := s.AcquireSlot()
		require.True(t, ok)
		_, _ = buf.ReadFrom(bytes.NewReader([]byte("seg")))
		require.NoError(t, s.CommitSlot(i, buf, int64(1000+i*2000), 2000, 1))
	}

	// Push one more gen=1 segment with a future timestamp before advancing clock.
	buf, ok := s.AcquireSlot()
	require.True(t, ok)
	_, _ = buf.ReadFrom(bytes.NewReader([]byte("seg")))
	require.NoError(t, s.CommitSlot(6, buf, int64(13000), 2000, 1))

	// Advance clock past all current segments so they're eligible and eviction triggers.
	clk.Set(time.UnixMilli(20000))

	// Push one more gen=1 segment to trigger eviction (CommitSlot calls evictOldLocked).
	buf, ok = s.AcquireSlot()
	require.True(t, ok)
	_, _ = buf.ReadFrom(bytes.NewReader([]byte("seg")))
	require.NoError(t, s.CommitSlot(7, buf, int64(21000), 2000, 1))

	// Advance clock past segment_7's timestamp so it becomes eligible too.
	clk.Set(time.UnixMilli(22000))

	// Push one more to wake the renderer after the clock advance.
	buf, ok = s.AcquireSlot()
	require.True(t, ok)
	_, _ = buf.ReadFrom(bytes.NewReader([]byte("seg")))
	require.NoError(t, s.CommitSlot(8, buf, int64(23000), 2000, 1))

	// Wait for the playlist renderer to produce a playlist with segment_7.
	require.Eventually(t, func() bool {
		p := s.CachedPlaylist()
		return p != "" && strings.Contains(p, "segment_7.m4s")
	}, 2*time.Second, 10*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/stream/1/media.m3u8", nil)
	rec := httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()

	// The gen 0→1 discontinuity has scrolled out of the window.
	// EXT-X-DISCONTINUITY-SEQUENCE should be at least 1.
	assert.Contains(t, body, "#EXT-X-DISCONTINUITY-SEQUENCE:1\n",
		"discontinuity sequence should increment when transition scrolls out; got:\n%s", body)

	// The window should contain only gen=1 segments (no discontinuity within window).
	assert.NotContains(t, body, "#EXT-X-DISCONTINUITY\n",
		"no discontinuity tag expected within window; got:\n%s", body)

	// Only gen=1 MAP should be in the window.
	assert.Contains(t, body, `#EXT-X-MAP:URI="init_1.mp4"`)
	assert.NotContains(t, body, `#EXT-X-MAP:URI="init_0.mp4"`)
}

func TestE2E_SubsequentInitConflictingMetadata(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	_, apiRouter, store, _ := testBothRoutersWithToken(t, clk)

	hdrs := initHeaders()
	rec := postInit(apiRouter, "1", "test-token", hdrs, []byte("init-gen0"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	// Subsequent init with conflicting bandwidth should be rejected.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/stream/1/init", bytes.NewReader([]byte("init-gen1")))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("X-SL-GENERATION", "1")
	req.Header.Set("X-SL-BANDWIDTH", "9999999") // conflicts with existing 4000000
	rec = httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusConflict, rec.Code)

	// Subsequent init with matching bandwidth should succeed.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/stream/1/init", bytes.NewReader([]byte("init-gen1")))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("X-SL-GENERATION", "1")
	req.Header.Set("X-SL-BANDWIDTH", "4000000") // matches existing
	rec = httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusCreated, rec.Code)
}

func TestE2E_SubsequentInitMalformedMetadata(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	_, apiRouter, store, _ := testBothRoutersWithToken(t, clk)

	hdrs := initHeaders()
	rec := postInit(apiRouter, "1", "test-token", hdrs, []byte("init-gen0"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	// Subsequent init with a malformed bandwidth header should be rejected.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/stream/1/init", bytes.NewReader([]byte("init-gen1")))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("X-SL-GENERATION", "1")
	req.Header.Set("X-SL-BANDWIDTH", "not-a-number")
	rec = httptest.NewRecorder()
	apiRouter.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestE2E_SubsequentInitPreviousGeneration(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	_, apiRouter, store, _ := testBothRoutersWithToken(t, clk)

	hdrs := initHeaders()
	rec := postInit(apiRouter, "1", "test-token", hdrs, []byte("init-gen0"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	// Add gen 2 (skipping 1).
	rec = postInitForGeneration(apiRouter, "1", "test-token", "2", []byte("init-gen2"))
	require.Equal(t, http.StatusCreated, rec.Code)

	// Try to add gen 1 (previous to max existing gen 2) → rejected.
	rec = postInitForGeneration(apiRouter, "1", "test-token", "1", []byte("init-gen1"))
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestE2E_NegativeGenerationOnInitRoute(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	streamRouter, _, store, _ := testBothRoutersWithToken(t, clk)

	meta := stream.Metadata{
		Bandwidth:          4000000,
		Codecs:             "avc1.64001f",
		Width:              1920,
		Height:             1080,
		FrameRate:          23.976,
		TargetDurationSecs: 2,
	}
	require.NoError(t, store.Init("1", meta, []byte("init"), 0, 10, 1024, 5, 2, config.DefaultMediaWindowSize))
	t.Cleanup(func() { store.Delete("1") })

	// Negative generation in init URL should return 400.
	req := httptest.NewRequest(http.MethodGet, "/stream/1/init_-1.mp4", nil)
	rec := httptest.NewRecorder()
	streamRouter.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
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

	// 3. Retrieve init.mp4 by generation.
	s := store.Get("myStream")
	require.NotNil(t, s)
	req := httptest.NewRequest(http.MethodGet, "/stream/myStream/init_0.mp4", nil)
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
