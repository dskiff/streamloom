package config

import (
	"os"
	"strings"
	"testing"

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

func TestParseStreamViewerTokenKeysSingle(t *testing.T) {
	clearViewerKeyEnv(t)
	key := strings.Repeat("a", MinTokenLength)
	t.Setenv("SL_STREAM_1_VIEWER_TOKEN_KEY", key)

	keys, err := parseStreamViewerTokenKeys()
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.Equal(t, []byte(key), keys["1"])
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
	assert.Equal(t, []byte(k1), keys["1"])
	assert.Equal(t, []byte(k2), keys["42"])
}

func TestParseStreamViewerTokenKeysAlphanumericIDAccepted(t *testing.T) {
	clearViewerKeyEnv(t)
	key := strings.Repeat("x", MinTokenLength)
	t.Setenv("SL_STREAM_MyStream1_VIEWER_TOKEN_KEY", key)

	keys, err := parseStreamViewerTokenKeys()
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.Equal(t, []byte(key), keys["MyStream1"])
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
	os.Setenv("SL_STREAM_5_VIEWER_TOKEN_KEY", key)

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

func TestGetViewerTokenKeyFound(t *testing.T) {
	want := []byte("supersecret")
	env := Env{
		STREAM_VIEWER_TOKEN_KEYS: map[string][]byte{"1": want},
	}
	got, ok := env.GetViewerTokenKey("1")
	require.True(t, ok)
	assert.Equal(t, want, got)
}

func TestGetViewerTokenKeyNotFound(t *testing.T) {
	env := Env{
		STREAM_VIEWER_TOKEN_KEYS: map[string][]byte{},
	}
	_, ok := env.GetViewerTokenKey("99")
	assert.False(t, ok)
}
