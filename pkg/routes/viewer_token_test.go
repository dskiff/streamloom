package routes

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	assert.Equal(t, exp, resp.ExpiresAtMs)

	// The minted token must verify against the same key.
	err := viewer.Verify(testViewerKey, clk.Now(), resp.Token)
	assert.NoError(t, err)
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
