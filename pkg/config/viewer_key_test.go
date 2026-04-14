package config

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/dskiff/streamloom/pkg/viewer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func clearViewerKeyEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(key, viewerTokenKeyPrefix) && strings.HasSuffix(key, viewerTokenKeySuffix) {
			os.Unsetenv(key)
		}
	}
}

// expectedViewerKeys returns the (playlist, segment) keys that should be
// produced by parseStreamViewerTokenKeys for (streamID, rawSecret).
func expectedViewerKeys(t *testing.T, rawSecret, streamID string) ViewerKeys {
	t.Helper()
	pk, err := viewer.DeriveKey([]byte(rawSecret), streamID, viewer.TypePlaylist)
	require.NoError(t, err)
	sk, err := viewer.DeriveKey([]byte(rawSecret), streamID, viewer.TypeSegment)
	require.NoError(t, err)
	return ViewerKeys{Playlist: pk, Segment: sk}
}

func TestParseStreamViewerTokenKeysSingle(t *testing.T) {
	clearViewerKeyEnv(t)
	key := strings.Repeat("a", MinTokenLength)
	t.Setenv("SL_STREAM_1_VIEWER_TOKEN_KEY", key)

	keys, err := parseStreamViewerTokenKeys()
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.Equal(t, expectedViewerKeys(t, key, "1"), keys["1"])
}

func TestParseStreamViewerTokenKeysMultiple(t *testing.T) {
	clearViewerKeyEnv(t)
	k1 := strings.Repeat("a", MinTokenLength)
	k2 := strings.Repeat("b", MinTokenLength)
	t.Setenv("SL_STREAM_1_VIEWER_TOKEN_KEY", k1)
	t.Setenv("SL_STREAM_42_VIEWER_TOKEN_KEY", k2)

	keys, err := parseStreamViewerTokenKeys()
	require.NoError(t, err)
	require.Len(t, keys, 2)
	assert.Equal(t, expectedViewerKeys(t, k1, "1"), keys["1"])
	assert.Equal(t, expectedViewerKeys(t, k2, "42"), keys["42"])
}

func TestParseStreamViewerTokenKeysAlphanumericIDAccepted(t *testing.T) {
	clearViewerKeyEnv(t)
	key := strings.Repeat("x", MinTokenLength)
	t.Setenv("SL_STREAM_MyStream1_VIEWER_TOKEN_KEY", key)

	keys, err := parseStreamViewerTokenKeys()
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.Equal(t, expectedViewerKeys(t, key, "MyStream1"), keys["MyStream1"])
}

func TestParseStreamViewerTokenKeysEmptyRejected(t *testing.T) {
	clearViewerKeyEnv(t)
	t.Setenv("SL_STREAM_1_VIEWER_TOKEN_KEY", "")

	_, err := parseStreamViewerTokenKeys()
	assert.Error(t, err)
}

func TestParseStreamViewerTokenKeysShortRejected(t *testing.T) {
	clearViewerKeyEnv(t)
	t.Setenv("SL_STREAM_1_VIEWER_TOKEN_KEY", "tooshort")

	_, err := parseStreamViewerTokenKeys()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too short")
}

func TestParseStreamViewerTokenKeysUnsetsEnvVars(t *testing.T) {
	clearViewerKeyEnv(t)
	key := strings.Repeat("z", MinTokenLength)
	// t.Setenv registers a cleanup to restore the previous value (which is
	// "unset" here after clearViewerKeyEnv), so even if the parser fails to
	// unset the variable the test-level cleanup will still leave the env
	// clean for sibling tests.
	t.Setenv("SL_STREAM_5_VIEWER_TOKEN_KEY", key)

	_, err := parseStreamViewerTokenKeys()
	require.NoError(t, err)

	assert.Empty(t, os.Getenv("SL_STREAM_5_VIEWER_TOKEN_KEY"),
		"env var SL_STREAM_5_VIEWER_TOKEN_KEY should be unset after parsing")
}

func TestParseStreamViewerTokenKeysNoKeys(t *testing.T) {
	clearViewerKeyEnv(t)

	keys, err := parseStreamViewerTokenKeys()
	require.NoError(t, err)
	assert.Empty(t, keys)
}

func TestParseStreamViewerTokenKeysOverlappingPrefixSuffix(t *testing.T) {
	clearViewerKeyEnv(t)
	// SL_STREAM_VIEWER_TOKEN_KEY matches both prefix and suffix but has no
	// stream ID between them. Must not panic; must be skipped.
	t.Setenv("SL_STREAM_VIEWER_TOKEN_KEY", strings.Repeat("q", MinTokenLength))

	keys, err := parseStreamViewerTokenKeys()
	require.NoError(t, err)
	assert.Empty(t, keys)
}

// Regression: push-token parsing must not misclassify a viewer-key env var.
func TestParseStreamTokensIgnoresViewerKeyVars(t *testing.T) {
	clearStreamEnv(t)
	clearViewerKeyEnv(t)
	t.Setenv("SL_STREAM_1_VIEWER_TOKEN_KEY", strings.Repeat("a", MinTokenLength))

	tokens, err := parseStreamTokens()
	require.NoError(t, err)
	assert.Empty(t, tokens, "viewer-key env var must not be classified as a push token")
}

// Regression: viewer-key parsing must not consume a push-token env var.
func TestParseStreamViewerTokenKeysIgnoresPushTokenVars(t *testing.T) {
	clearStreamEnv(t)
	clearViewerKeyEnv(t)
	t.Setenv("SL_STREAM_1_TOKEN", strings.Repeat("a", MinTokenLength))

	keys, err := parseStreamViewerTokenKeys()
	require.NoError(t, err)
	assert.Empty(t, keys)
}

// TestParseStreamViewerTokenKeysDerivesDistinctClassKeys asserts that the
// parser populates both Playlist and Segment fields with distinct bytes,
// and that neither equals the raw env secret — the hardening property
// that motivates pre-deriving at parse time.
func TestParseStreamViewerTokenKeysDerivesDistinctClassKeys(t *testing.T) {
	clearViewerKeyEnv(t)
	raw := strings.Repeat("a", MinTokenLength)
	t.Setenv("SL_STREAM_1_VIEWER_TOKEN_KEY", raw)

	keys, err := parseStreamViewerTokenKeys()
	require.NoError(t, err)
	vk := keys["1"]
	require.NotEmpty(t, vk.Playlist)
	require.NotEmpty(t, vk.Segment)
	assert.False(t, bytes.Equal(vk.Playlist, vk.Segment),
		"playlist and segment keys must be distinct")
	assert.False(t, bytes.Equal(vk.Playlist, []byte(raw)),
		"derived key must not equal raw env secret")
	assert.False(t, bytes.Equal(vk.Segment, []byte(raw)),
		"derived key must not equal raw env secret")
}

// TestParseStreamViewerTokenKeysCrossStreamIsolation asserts that two
// streams sharing the same raw env secret still derive distinct keys —
// the KDF-backed cross-stream binding that the refactor adds.
func TestParseStreamViewerTokenKeysCrossStreamIsolation(t *testing.T) {
	clearViewerKeyEnv(t)
	raw := strings.Repeat("a", MinTokenLength)
	t.Setenv("SL_STREAM_1_VIEWER_TOKEN_KEY", raw)
	t.Setenv("SL_STREAM_2_VIEWER_TOKEN_KEY", raw)

	keys, err := parseStreamViewerTokenKeys()
	require.NoError(t, err)
	require.Len(t, keys, 2)
	assert.False(t, bytes.Equal(keys["1"].Playlist, keys["2"].Playlist),
		"streams with identical raw secret must have distinct playlist keys")
	assert.False(t, bytes.Equal(keys["1"].Segment, keys["2"].Segment),
		"streams with identical raw secret must have distinct segment keys")
}

func TestGetViewerKeysFound(t *testing.T) {
	want := ViewerKeys{Playlist: []byte("p"), Segment: []byte("s")}
	env := Env{
		STREAM_VIEWER_TOKEN_KEYS: map[string]ViewerKeys{"1": want},
	}
	got, ok := env.GetViewerKeys("1")
	require.True(t, ok)
	assert.Equal(t, want, got)
}

func TestGetViewerKeysNotFound(t *testing.T) {
	env := Env{
		STREAM_VIEWER_TOKEN_KEYS: map[string]ViewerKeys{},
	}
	_, ok := env.GetViewerKeys("99")
	assert.False(t, ok)
}
