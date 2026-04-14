package routes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/dskiff/streamloom/pkg/viewer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// postViewerToken POSTs a JSON body to /api/v1/stream/{streamID}/viewer_token
// with the given push-token Authorization. Returns the httptest recorder.
func postViewerToken(router http.Handler, streamID, pushToken string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/stream/"+streamID+"/viewer_token", bytes.NewReader(body))
	if pushToken != "" {
		req.Header.Set("Authorization", "Bearer "+pushToken)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestViewerToken_Success(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	router, _, _, _ := testAPIRouterWithViewerKey(t, clk)

	exp := clk.Now().Add(1 * time.Hour).UnixMilli()
	body, _ := json.Marshal(map[string]any{"expires_at_ms": exp})

	rec := postViewerToken(router, "1", "test-token", body)
	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	var resp struct {
		Token       string `json:"token"`
		ExpiresAtMs int64  `json:"expires_at_ms"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// Server echoes the minute-aligned expiry actually encoded in the token.
	// 1_700_000_000_000 is 20s past a minute boundary, so +1h rounds down 20s.
	const msPerMin = int64(60_000)
	assert.Equal(t, (exp/msPerMin)*msPerMin, resp.ExpiresAtMs,
		"echoed expiry must be floored to minute boundary")
	assert.LessOrEqual(t, resp.ExpiresAtMs, exp,
		"echoed expiry must never exceed the requested value")

	// The minted token must verify under the playlist-derived signing
	// key — the operator-grant class accepted on all stream routes. It
	// must NOT verify under the segment-derived key (KDF-backed
	// cross-type isolation).
	keys := testViewerKeys(t, "1")
	err := viewer.Verify(keys.Playlist, clk.Now(), resp.Token)
	assert.NoError(t, err)
	err = viewer.Verify(keys.Segment, clk.Now(), resp.Token)
	assert.ErrorIs(t, err, viewer.ErrBadMAC,
		"operator-grant token must not verify under the segment-derived key")

	// The minted token is the shorter 28-char form (21 bytes payload).
	assert.Len(t, resp.Token, viewer.EncodedTokenLen)
}

func TestViewerToken_NoKeyConfigured(t *testing.T) {
	// The default API router has no viewer key configured.
	router, _, _, _ := testAPIRouterWithToken(t, clock.Real{})

	body, _ := json.Marshal(map[string]any{"expires_at_ms": time.Now().Add(time.Hour).UnixMilli()})
	rec := postViewerToken(router, "1", "test-token", body)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestViewerToken_MissingPushAuth(t *testing.T) {
	router, _, _, _ := testAPIRouterWithViewerKey(t, clock.Real{})

	body, _ := json.Marshal(map[string]any{"expires_at_ms": time.Now().Add(time.Hour).UnixMilli()})
	rec := postViewerToken(router, "1", "", body)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestViewerToken_InvalidPushAuth(t *testing.T) {
	router, _, _, _ := testAPIRouterWithViewerKey(t, clock.Real{})

	body, _ := json.Marshal(map[string]any{"expires_at_ms": time.Now().Add(time.Hour).UnixMilli()})
	rec := postViewerToken(router, "1", "wrong-token", body)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestViewerToken_InvalidStreamID(t *testing.T) {
	router, _, _, _ := testAPIRouterWithViewerKey(t, clock.Real{})

	body, _ := json.Marshal(map[string]any{"expires_at_ms": time.Now().Add(time.Hour).UnixMilli()})
	rec := postViewerToken(router, "a.b", "test-token", body)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestViewerToken_ExpiryInPast(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	router, _, _, _ := testAPIRouterWithViewerKey(t, clk)

	body, _ := json.Marshal(map[string]any{"expires_at_ms": clk.Now().Add(-time.Second).UnixMilli()})
	rec := postViewerToken(router, "1", "test-token", body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestViewerToken_ExpiryEqualsNow(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	router, _, _, _ := testAPIRouterWithViewerKey(t, clk)

	body, _ := json.Marshal(map[string]any{"expires_at_ms": clk.Now().UnixMilli()})
	rec := postViewerToken(router, "1", "test-token", body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestViewerToken_TTLBelowMinimum covers values just under the 5-minute floor.
// The handler must reject anything whose minute-aligned TTL is less than the
// minimum, including cases where the requested raw TTL is above 5 minutes but
// rounding brings it below.
func TestViewerToken_TTLBelowMinimum(t *testing.T) {
	// Minute-aligned now so the expectations are easy to reason about.
	clk := clock.NewMock(time.UnixMilli(60_000 * 28_333_333))
	router, _, _, _ := testAPIRouterWithViewerKey(t, clk)

	cases := []time.Duration{
		0,
		1 * time.Second,
		1 * time.Minute,
		4*time.Minute + 59*time.Second,
		// Rounds down to exactly 4 minutes (below the floor).
		5*time.Minute - 1*time.Millisecond,
	}
	for _, d := range cases {
		body, _ := json.Marshal(map[string]any{"expires_at_ms": clk.Now().Add(d).UnixMilli()})
		rec := postViewerToken(router, "1", "test-token", body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "expected 400 for TTL=%s", d)
	}
}

// TestViewerToken_TTLExactlyMinimum asserts that a request whose minute-
// aligned TTL equals the 5-minute floor is accepted.
func TestViewerToken_TTLExactlyMinimum(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(60_000 * 28_333_333))
	router, _, _, _ := testAPIRouterWithViewerKey(t, clk)

	exp := clk.Now().Add(5 * time.Minute).UnixMilli()
	body, _ := json.Marshal(map[string]any{"expires_at_ms": exp})
	rec := postViewerToken(router, "1", "test-token", body)
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp struct {
		ExpiresAtMs int64 `json:"expires_at_ms"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, exp, resp.ExpiresAtMs, "minute-aligned input should echo unchanged")
}

func TestViewerToken_EmptyBody(t *testing.T) {
	router, _, _, _ := testAPIRouterWithViewerKey(t, clock.Real{})

	rec := postViewerToken(router, "1", "test-token", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestViewerToken_InvalidJSON(t *testing.T) {
	router, _, _, _ := testAPIRouterWithViewerKey(t, clock.Real{})

	rec := postViewerToken(router, "1", "test-token", []byte("{not json"))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestViewerToken_UnknownField(t *testing.T) {
	router, _, _, _ := testAPIRouterWithViewerKey(t, clock.Real{})

	rec := postViewerToken(router, "1", "test-token", []byte(`{"expires_at_ms": 9999999999999, "attacker_field": 1}`))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestViewerToken_TrailingData asserts the handler rejects a body containing
// trailing JSON after the primary object. json.Decoder.Decode consumes a
// single value and silently ignores anything after it; without an explicit
// check an attacker could smuggle extra JSON past the decoder.
func TestViewerToken_TrailingData(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	router, _, _, _ := testAPIRouterWithViewerKey(t, clk)

	exp := clk.Now().Add(time.Hour).UnixMilli()
	body := []byte(fmt.Sprintf(`{"expires_at_ms": %d}{"extra":1}`, exp))
	rec := postViewerToken(router, "1", "test-token", body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestViewerToken_TrailingWhitespace asserts that trailing whitespace (which
// is harmless and common from naive encoders) does NOT cause a 400. Only
// meaningful trailing JSON is rejected.
func TestViewerToken_TrailingWhitespace(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	router, _, _, _ := testAPIRouterWithViewerKey(t, clk)

	exp := clk.Now().Add(time.Hour).UnixMilli()
	body := []byte(fmt.Sprintf("{\"expires_at_ms\": %d}\n\t  \n", exp))
	rec := postViewerToken(router, "1", "test-token", body)
	assert.Equal(t, http.StatusCreated, rec.Code)
}

// TestViewerToken_ExpiryOverflowsUint32Minutes asserts that a client-supplied
// expiration so large that its minute value overflows uint32 (the encoded
// field's size) is rejected with 400 rather than 500. This exp passes the
// min-TTL check, reaches viewer.Mint, and returns viewer.ErrMalformed —
// without explicit mapping the handler would surface it as an internal error,
// making a client-triggerable condition look like a server failure.
func TestViewerToken_ExpiryOverflowsUint32Minutes(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1_700_000_000_000))
	router, _, _, _ := testAPIRouterWithViewerKey(t, clk)

	// One minute past the uint32 range, in ms.
	const msPerMin = int64(60_000)
	exp := (int64(math.MaxUint32) + 1) * msPerMin
	body, _ := json.Marshal(map[string]any{"expires_at_ms": exp})
	rec := postViewerToken(router, "1", "test-token", body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestViewerToken_BodyTooLarge asserts that a request body exceeding the
// MaxViewerTokenRequestBytes cap is rejected with 413 rather than 400.
// Parallels the 413 behavior of /init and /segment.
func TestViewerToken_BodyTooLarge(t *testing.T) {
	router, _, _, _ := testAPIRouterWithViewerKey(t, clock.Real{})

	// A payload well over the 1 KiB cap: valid-looking JSON padded with a
	// huge unused string. MaxBytesReader trips before Decode can finish.
	padding := strings.Repeat("A", MaxViewerTokenRequestBytes+1)
	body := []byte(`{"expires_at_ms": 9999999999999, "_pad": "` + padding + `"}`)
	rec := postViewerToken(router, "1", "test-token", body)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}
