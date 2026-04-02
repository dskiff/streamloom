package routes

import (
	"crypto/sha256"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/dskiff/streamloom/pkg/config"
	"github.com/dskiff/streamloom/pkg/stream"
	"github.com/dskiff/streamloom/pkg/watcher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Auth middleware tests ---

func TestAuth_MissingToken(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	hdrs := initHeaders()
	rec := postInit(router, "1", "", hdrs, []byte("init-data"))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuth_InvalidToken(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	hdrs := initHeaders()
	rec := postInit(router, "1", "wrong-token", hdrs, []byte("init-data"))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuth_UnconfiguredStream(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	hdrs := initHeaders()
	// Stream 999 has no configured token.
	rec := postInit(router, "999", "test-token", hdrs, []byte("init-data"))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuth_InvalidStreamID(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	hdrs := initHeaders()
	rec := postInit(router, "a.b", "test-token", hdrs, []byte("init-data"))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAuth_ValidToken(t *testing.T) {
	router, store, _, _ := testAPIRouterWithToken(t, clock.Real{})

	hdrs := initHeaders()
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	assert.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })
}

// --- POST /api/v1/stream/{streamID}/init tests ---

func TestPostInit_Success(t *testing.T) {
	router, store, _, _ := testAPIRouterWithToken(t, clock.Real{})

	hdrs := initHeaders()
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	assert.Equal(t, http.StatusCreated, rec.Code)

	s := store.Get("1")
	require.NotNil(t, s)
	t.Cleanup(func() { store.Delete("1") })

	meta := s.Metadata()
	assert.Equal(t, 4000000, meta.Bandwidth)
	assert.Equal(t, "avc1.64001f", meta.Codecs)
	assert.Equal(t, 1920, meta.Width)
	assert.Equal(t, 1080, meta.Height)
	assert.InDelta(t, 23.976, meta.FrameRate, 0.001)
	assert.Equal(t, 2, meta.TargetDurationSecs)
}

func TestPostInit_MissingBandwidth(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	hdrs := initHeaders()
	delete(hdrs, "X-SL-BANDWIDTH")
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostInit_InvalidBandwidth(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	hdrs := initHeaders()
	hdrs["X-SL-BANDWIDTH"] = "-1"
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostInit_MissingCodecs(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	hdrs := initHeaders()
	hdrs["X-SL-CODECS"] = ""
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostInit_MissingResolution(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	hdrs := initHeaders()
	delete(hdrs, "X-SL-RESOLUTION")
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostInit_InvalidResolution(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	hdrs := initHeaders()
	hdrs["X-SL-RESOLUTION"] = "notvalid"
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostInit_OversizedResolution(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	hdrs := initHeaders()
	hdrs["X-SL-RESOLUTION"] = "99999x99999"
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostInit_MissingFramerate(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	hdrs := initHeaders()
	delete(hdrs, "X-SL-FRAMERATE")
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostInit_InvalidFramerate(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	for _, val := range []string{"0", "-1", "NaN", "Inf"} {
		hdrs := initHeaders()
		hdrs["X-SL-FRAMERATE"] = val
		rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
		assert.Equal(t, http.StatusBadRequest, rec.Code, "framerate=%s", val)
	}
}

func TestPostInit_MissingTargetDuration(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	hdrs := initHeaders()
	delete(hdrs, "X-SL-TARGET-DURATION")
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostInit_EmptyBody(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	hdrs := initHeaders()
	rec := postInit(router, "1", "test-token", hdrs, []byte{})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostInit_OversizedBody(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	hdrs := initHeaders()
	body := make([]byte, stream.MaxInitBytes+1)
	rec := postInit(router, "1", "test-token", hdrs, body)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestPostInit_MissingSegmentCap(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	hdrs := initHeaders()
	delete(hdrs, "X-SL-SEGMENT-CAP")
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostInit_MissingSegmentBytes(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	hdrs := initHeaders()
	delete(hdrs, "X-SL-SEGMENT-BYTES")
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostInit_MissingBackwardBufferSize(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	hdrs := initHeaders()
	delete(hdrs, "X-SL-BACKWARD-BUFFER-SIZE")
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostInit_ExceedsMaxBufferBytes(t *testing.T) {
	// With STREAM_MAX_BUFFER_BYTES=1024 and (segmentCap + workingSpace) * segmentBytes
	// far exceeding that, the request should be rejected.
	store := stream.NewStore(clock.Real{})
	l := slog.Default()
	env := config.Env{
		STREAM_MAX_BUFFER_BYTES: 1024, // very small
		BUFFER_WORKING_SPACE:    2,
		STREAM_TOKENS: map[string]config.TokenDigest{
			"1": sha256.Sum256([]byte("Bearer test-token")),
		},
	}
	tracker := watcher.NewTracker(clock.Real{})
	router := API(l, env, store, nil, tracker)

	hdrs := initHeaders()
	hdrs["X-SL-SEGMENT-CAP"] = "100"
	hdrs["X-SL-SEGMENT-BYTES"] = "1024"
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostInit_BufferSizeOverflowsInt64(t *testing.T) {
	// On 64-bit platforms strconv.Atoi can parse values up to math.MaxInt64.
	// Verify that the overflow guard rejects inputs whose product exceeds int64.
	if strconv.IntSize < 64 {
		t.Skip("overflow guard targets 64-bit; Atoi range prevents this on 32-bit")
	}

	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})
	hdrs := initHeaders()
	// These values are individually valid ints on 64-bit, but their product
	// overflows int64: (4611686018427387903 + 20) * 3 > math.MaxInt64.
	hdrs["X-SL-SEGMENT-CAP"] = "4611686018427387903"
	hdrs["X-SL-SEGMENT-BYTES"] = "3"
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostInit_ReInitClearsSegments(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _, _ := testAPIRouterWithToken(t, clk)

	hdrs := initHeaders()
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data-v1"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	s := store.Get("1")
	require.NotNil(t, s)
	commitSegment(t, s, 0, []byte("seg"), 5000)

	// Re-initialize should clear segments.
	rec = postInit(router, "1", "test-token", hdrs, []byte("init-data-v2"))
	require.Equal(t, http.StatusCreated, rec.Code)

	s = store.Get("1")
	require.NotNil(t, s)
	assert.Equal(t, int64(0), s.TotalSegmentCount())
}

// --- POST /api/v1/stream/{streamID}/segment tests ---

func TestPostSegment_Success(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _, _ := testAPIRouterWithToken(t, clk)

	hdrs := initHeaders()
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	rec = postSegment(router, "1", "test-token", "0", "5000", "2000", []byte("segment-data"))
	assert.Equal(t, http.StatusCreated, rec.Code)

	s := store.Get("1")
	require.NotNil(t, s)
	assert.Equal(t, int64(1), s.TotalSegmentCount())
}

func TestPostSegment_StreamNotFound(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	rec := postSegment(router, "1", "test-token", "0", "5000", "2000", []byte("segment-data"))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestPostSegment_MissingIndex(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _, _ := testAPIRouterWithToken(t, clk)
	hdrs := initHeaders()
	postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	t.Cleanup(func() { store.Delete("1") })

	rec := postSegment(router, "1", "test-token", "", "5000", "2000", []byte("data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostSegment_MissingTimestamp(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _, _ := testAPIRouterWithToken(t, clk)
	hdrs := initHeaders()
	postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	t.Cleanup(func() { store.Delete("1") })

	rec := postSegment(router, "1", "test-token", "0", "", "2000", []byte("data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostSegment_MissingDuration(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _, _ := testAPIRouterWithToken(t, clk)
	hdrs := initHeaders()
	postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	t.Cleanup(func() { store.Delete("1") })

	rec := postSegment(router, "1", "test-token", "0", "5000", "", []byte("data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostSegment_ZeroDuration(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _, _ := testAPIRouterWithToken(t, clk)
	hdrs := initHeaders()
	postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	t.Cleanup(func() { store.Delete("1") })

	rec := postSegment(router, "1", "test-token", "0", "5000", "0", []byte("data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostSegment_EmptyBody(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _, _ := testAPIRouterWithToken(t, clk)
	hdrs := initHeaders()
	postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	t.Cleanup(func() { store.Delete("1") })

	rec := postSegment(router, "1", "test-token", "0", "5000", "2000", []byte{})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostSegment_DuplicateIndex(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _, _ := testAPIRouterWithToken(t, clk)
	hdrs := initHeaders()
	postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	t.Cleanup(func() { store.Delete("1") })

	rec := postSegment(router, "1", "test-token", "0", "5000", "2000", []byte("data"))
	require.Equal(t, http.StatusCreated, rec.Code)

	rec = postSegment(router, "1", "test-token", "0", "7000", "2000", []byte("data"))
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestPostSegment_TimestampInPast(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _, _ := testAPIRouterWithToken(t, clk)

	hdrs := initHeaders()
	postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	t.Cleanup(func() { store.Delete("1") })

	// Push first segment so the stream is non-empty.
	rec := postSegment(router, "1", "test-token", "0", "5000", "2000", []byte("data"))
	require.Equal(t, http.StatusCreated, rec.Code)

	// Advance time past the next segment's timestamp.
	clk.Set(time.UnixMilli(10000))

	rec = postSegment(router, "1", "test-token", "1", "1000", "2000", []byte("data"))
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestPostSegment_OversizedBody(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _, _ := testAPIRouterWithToken(t, clk)
	hdrs := initHeaders()
	// Segment bytes is 1024, so a body larger than that should be rejected.
	postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	t.Cleanup(func() { store.Delete("1") })

	bigBody := make([]byte, 2048)
	rec := postSegment(router, "1", "test-token", "0", "5000", "2000", bigBody)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

// --- DELETE /api/v1/stream/{streamID} tests ---

func TestDelete_Success(t *testing.T) {
	router, store, _, _ := testAPIRouterWithToken(t, clock.Real{})

	hdrs := initHeaders()
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	require.Equal(t, http.StatusCreated, rec.Code)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/stream/1/", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Nil(t, store.Get("1"))
}

func TestDelete_NotFound(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/stream/1/", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestDelete_Unauthorized(t *testing.T) {
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/stream/1/", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// --- POST /api/v1/stream/{streamID}/segment generation tests ---

func TestPostSegment_GenerationAccepted(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _, _ := testAPIRouterWithToken(t, clk)

	hdrs := initHeaders()
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	rec = postSegmentWithGen(router, "1", "test-token", "0", "5000", "2000", "1", []byte("data"))
	assert.Equal(t, http.StatusCreated, rec.Code)
}

func TestPostSegment_StaleGeneration(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _, _ := testAPIRouterWithToken(t, clk)

	hdrs := initHeaders()
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	// Push gen=5.
	rec = postSegmentWithGen(router, "1", "test-token", "0", "5000", "2000", "5", []byte("data"))
	require.Equal(t, http.StatusCreated, rec.Code)

	// Push gen=2 → stale → 409.
	rec = postSegmentWithGen(router, "1", "test-token", "1", "7000", "2000", "2", []byte("data"))
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestPostSegment_InvalidGeneration(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _, _ := testAPIRouterWithToken(t, clk)

	hdrs := initHeaders()
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	rec = postSegmentWithGen(router, "1", "test-token", "0", "5000", "2000", "abc", []byte("data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostSegment_NegativeGeneration(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _, _ := testAPIRouterWithToken(t, clk)

	hdrs := initHeaders()
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	rec = postSegmentWithGen(router, "1", "test-token", "0", "5000", "2000", "-1", []byte("data"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostSegment_MissingGenerationDefaultsZero(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(0))
	router, store, _, _ := testAPIRouterWithToken(t, clk)

	hdrs := initHeaders()
	rec := postInit(router, "1", "test-token", hdrs, []byte("init-data"))
	require.Equal(t, http.StatusCreated, rec.Code)
	t.Cleanup(func() { store.Delete("1") })

	// No X-SL-GENERATION header → defaults to 0 → 201.
	rec = postSegment(router, "1", "test-token", "0", "5000", "2000", []byte("data"))
	assert.Equal(t, http.StatusCreated, rec.Code)
}
