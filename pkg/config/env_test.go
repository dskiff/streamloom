package config

import (
	"crypto/sha256"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func clearStreamEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(key, streamTokenPrefix) && strings.HasSuffix(key, streamTokenSuffix) {
			os.Unsetenv(key)
		}
	}
}

func TestParseStreamTokensSingle(t *testing.T) {
	clearStreamEnv(t)
	t.Setenv("SL_STREAM_1_TOKEN", "abcdefghijklmnopqrstuvwxyz123456")

	tokens, err := parseStreamTokens()
	require.NoError(t, err)
	require.Len(t, tokens, 1)

	expected := sha256.Sum256([]byte("Bearer abcdefghijklmnopqrstuvwxyz123456"))
	assert.Equal(t, expected, tokens["1"], "token digest mismatch")
}

func TestParseStreamTokensMultiple(t *testing.T) {
	clearStreamEnv(t)
	t.Setenv("SL_STREAM_1_TOKEN", "alpha-token-value-that-is-32chars")
	t.Setenv("SL_STREAM_42_TOKEN", "bravo-token-value-that-is-32chars")

	tokens, err := parseStreamTokens()
	require.NoError(t, err)
	require.Len(t, tokens, 2)

	exp1 := sha256.Sum256([]byte("Bearer alpha-token-value-that-is-32chars"))
	assert.Equal(t, exp1, tokens["1"], "stream 1 token digest mismatch")

	exp42 := sha256.Sum256([]byte("Bearer bravo-token-value-that-is-32chars"))
	assert.Equal(t, exp42, tokens["42"], "stream 42 token digest mismatch")
}

func TestParseStreamTokensAlphanumericIDAccepted(t *testing.T) {
	clearStreamEnv(t)
	t.Setenv("SL_STREAM_abc_TOKEN", "secrettoken-that-is-at-least-32c")

	tokens, err := parseStreamTokens()
	require.NoError(t, err)
	require.Len(t, tokens, 1)

	expected := sha256.Sum256([]byte("Bearer secrettoken-that-is-at-least-32c"))
	assert.Equal(t, expected, tokens["abc"], "token digest mismatch")
}

func TestParseStreamTokensMixedCaseIDAccepted(t *testing.T) {
	clearStreamEnv(t)
	t.Setenv("SL_STREAM_MyStream1_TOKEN", "secrettoken-that-is-at-least-32c")

	tokens, err := parseStreamTokens()
	require.NoError(t, err)
	require.Len(t, tokens, 1)

	expected := sha256.Sum256([]byte("Bearer secrettoken-that-is-at-least-32c"))
	assert.Equal(t, expected, tokens["MyStream1"], "token digest mismatch")
}

func TestParseStreamTokensEmptyTokenRejected(t *testing.T) {
	clearStreamEnv(t)
	t.Setenv("SL_STREAM_1_TOKEN", "")

	_, err := parseStreamTokens()
	assert.Error(t, err, "expected error for empty token value")
}

func TestParseStreamTokensShortTokenRejected(t *testing.T) {
	clearStreamEnv(t)
	t.Setenv("SL_STREAM_1_TOKEN", "tooshort")

	_, err := parseStreamTokens()
	require.Error(t, err, "expected error for short token value")
	assert.Contains(t, err.Error(), "too short")
}

func TestParseStreamTokensMinLengthTokenAccepted(t *testing.T) {
	clearStreamEnv(t)
	token := strings.Repeat("a", MinTokenLength)
	t.Setenv("SL_STREAM_1_TOKEN", token)

	tokens, err := parseStreamTokens()
	require.NoError(t, err)
	require.Len(t, tokens, 1)

	expected := sha256.Sum256([]byte("Bearer " + token))
	assert.Equal(t, expected, tokens["1"], "token digest mismatch")
}

func TestParseStreamTokensUnsetsEnvVars(t *testing.T) {
	clearStreamEnv(t)
	os.Setenv("SL_STREAM_5_TOKEN", "secrettoken-that-is-at-least-32c")

	_, err := parseStreamTokens()
	require.NoError(t, err)

	assert.Empty(t, os.Getenv("SL_STREAM_5_TOKEN"), "env var SL_STREAM_5_TOKEN not unset")
}

func TestParseStreamTokensNoTokens(t *testing.T) {
	clearStreamEnv(t)

	tokens, err := parseStreamTokens()
	require.NoError(t, err)
	assert.Empty(t, tokens)
}

func TestParseStreamTokensOverlappingPrefixSuffix(t *testing.T) {
	clearStreamEnv(t)
	// SL_STREAM_TOKEN matches both prefix and suffix but has no stream ID
	// between them (prefix + suffix overlap). Must not panic.
	t.Setenv("SL_STREAM_TOKEN", "irrelevant-value-that-is-long-enough")

	tokens, err := parseStreamTokens()
	require.NoError(t, err)
	assert.Empty(t, tokens, "overlapping prefix/suffix var should be silently skipped")
}

func TestExpectedTokenFound(t *testing.T) {
	digest := sha256.Sum256([]byte("Bearer tok1"))
	env := Env{
		STREAM_TOKENS: map[string]TokenDigest{
			"1": digest,
		},
	}
	tok, ok := env.GetStreamToken("1")
	require.True(t, ok, "expected token to be found")
	assert.Equal(t, digest, tok, "token digest mismatch")
}

func TestExpectedTokenNotFound(t *testing.T) {
	env := Env{
		STREAM_TOKENS: map[string]TokenDigest{},
	}
	_, ok := env.GetStreamToken("99")
	assert.False(t, ok, "expected token not to be found")
}

func TestValidateStreamID(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{"valid numeric", "42", false},
		{"valid alpha", "abc", false},
		{"valid alphanumeric", "stream1", false},
		{"valid mixed case", "MyStream", false},
		{"empty", "", true},
		{"contains hyphen", "my-stream", true},
		{"contains underscore", "my_stream", true},
		{"contains dot", "my.stream", true},
		{"contains slash", "my/stream", true},
		{"contains space", "my stream", true},
		{"too long", strings.Repeat("a", MaxStreamIDLength+1), true},
		{"max length", strings.Repeat("a", MaxStreamIDLength), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateStreamID(tt.id)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestParseStreamMaxBufferBytesDefault(t *testing.T) {
	t.Setenv("SL_STREAM_MAX_BUFFER_BYTES", "")

	v, err := parseStreamMaxBufferBytes()
	require.NoError(t, err)
	assert.Equal(t, DefaultStreamMaxBufferBytes, v)
}

func TestParseStreamMaxBufferBytesCustom(t *testing.T) {
	t.Setenv("SL_STREAM_MAX_BUFFER_BYTES", "524288000")

	v, err := parseStreamMaxBufferBytes()
	require.NoError(t, err)
	assert.Equal(t, int64(524288000), v)
}

func TestParseStreamMaxBufferBytesInvalid(t *testing.T) {
	t.Setenv("SL_STREAM_MAX_BUFFER_BYTES", "not-a-number")

	_, err := parseStreamMaxBufferBytes()
	assert.Error(t, err, "expected error for non-numeric value")
}

func TestParseStreamMaxBufferBytesZeroRejected(t *testing.T) {
	t.Setenv("SL_STREAM_MAX_BUFFER_BYTES", "0")

	_, err := parseStreamMaxBufferBytes()
	assert.Error(t, err, "expected error for zero value")
}

func TestParseStreamMaxBufferBytesNegativeRejected(t *testing.T) {
	t.Setenv("SL_STREAM_MAX_BUFFER_BYTES", "-100")

	_, err := parseStreamMaxBufferBytes()
	assert.Error(t, err, "expected error for negative value")
}

func TestParseBufferWorkingSpaceDefault(t *testing.T) {
	t.Setenv("SL_BUFFER_WORKING_SPACE", "")

	v, err := parseBufferWorkingSpace()
	require.NoError(t, err)
	assert.Equal(t, DefaultBufferWorkingSpace, v)
}

func TestParseBufferWorkingSpaceCustom(t *testing.T) {
	t.Setenv("SL_BUFFER_WORKING_SPACE", "50")

	v, err := parseBufferWorkingSpace()
	require.NoError(t, err)
	assert.Equal(t, 50, v)
}

func TestParseBufferWorkingSpaceZeroAllowed(t *testing.T) {
	t.Setenv("SL_BUFFER_WORKING_SPACE", "0")

	v, err := parseBufferWorkingSpace()
	require.NoError(t, err)
	assert.Equal(t, 0, v)
}

func TestParseBufferWorkingSpaceInvalid(t *testing.T) {
	t.Setenv("SL_BUFFER_WORKING_SPACE", "abc")

	_, err := parseBufferWorkingSpace()
	assert.Error(t, err, "expected error for non-numeric value")
}

func TestParseBufferWorkingSpaceNegativeRejected(t *testing.T) {
	t.Setenv("SL_BUFFER_WORKING_SPACE", "-1")

	_, err := parseBufferWorkingSpace()
	assert.Error(t, err, "expected error for negative value")
}

func TestParseTrustedProxiesEmpty(t *testing.T) {
	t.Setenv("SL_TRUSTED_PROXIES", "")

	nets, err := parseTrustedProxies()
	require.NoError(t, err)
	assert.Nil(t, nets)
}

func TestParseTrustedProxiesUnset(t *testing.T) {
	os.Unsetenv("SL_TRUSTED_PROXIES")

	nets, err := parseTrustedProxies()
	require.NoError(t, err)
	assert.Nil(t, nets)
}

func TestParseTrustedProxiesSingle(t *testing.T) {
	t.Setenv("SL_TRUSTED_PROXIES", "10.0.0.0/8")

	nets, err := parseTrustedProxies()
	require.NoError(t, err)
	require.Len(t, nets, 1)
	assert.Equal(t, "10.0.0.0/8", nets[0].String())
}

func TestParseTrustedProxiesMultiple(t *testing.T) {
	t.Setenv("SL_TRUSTED_PROXIES", "10.0.0.0/8, 172.16.0.0/12")

	nets, err := parseTrustedProxies()
	require.NoError(t, err)
	require.Len(t, nets, 2)
	assert.Equal(t, "10.0.0.0/8", nets[0].String())
	assert.Equal(t, "172.16.0.0/12", nets[1].String())
}

func TestParseTrustedProxiesInvalidCIDR(t *testing.T) {
	t.Setenv("SL_TRUSTED_PROXIES", "not-a-cidr")

	_, err := parseTrustedProxies()
	assert.Error(t, err, "expected error for invalid CIDR")
}

func TestParseTrustedProxiesTrailingComma(t *testing.T) {
	t.Setenv("SL_TRUSTED_PROXIES", "10.0.0.0/8,")

	nets, err := parseTrustedProxies()
	require.NoError(t, err)
	require.Len(t, nets, 1)
}

func TestParsePortDefault(t *testing.T) {
	t.Setenv("SL_STREAM_PORT", "")

	v, err := parsePort("SL_STREAM_PORT", DefaultStreamPort)
	require.NoError(t, err)
	assert.Equal(t, DefaultStreamPort, v)
}

func TestParsePortCustom(t *testing.T) {
	t.Setenv("SL_STREAM_PORT", "9090")

	v, err := parsePort("SL_STREAM_PORT", DefaultStreamPort)
	require.NoError(t, err)
	assert.Equal(t, 9090, v)
}

func TestParsePortInvalid(t *testing.T) {
	t.Setenv("SL_STREAM_PORT", "abc")

	_, err := parsePort("SL_STREAM_PORT", DefaultStreamPort)
	assert.Error(t, err)
}

func TestParsePortZeroRejected(t *testing.T) {
	t.Setenv("SL_STREAM_PORT", "0")

	_, err := parsePort("SL_STREAM_PORT", DefaultStreamPort)
	assert.Error(t, err)
}

func TestParsePortNegativeRejected(t *testing.T) {
	t.Setenv("SL_STREAM_PORT", "-1")

	_, err := parsePort("SL_STREAM_PORT", DefaultStreamPort)
	assert.Error(t, err)
}

func TestParsePortAbove65535Rejected(t *testing.T) {
	t.Setenv("SL_STREAM_PORT", "70000")

	_, err := parsePort("SL_STREAM_PORT", DefaultStreamPort)
	assert.Error(t, err)
}

func TestParseBindAddrEmpty(t *testing.T) {
	t.Setenv("SL_BIND_ADDR", "")

	v, err := parseBindAddr()
	require.NoError(t, err)
	assert.Empty(t, v)
}

func TestParseBindAddrUnset(t *testing.T) {
	os.Unsetenv("SL_BIND_ADDR")

	v, err := parseBindAddr()
	require.NoError(t, err)
	assert.Empty(t, v)
}

func TestParseBindAddrValidIPv4(t *testing.T) {
	t.Setenv("SL_BIND_ADDR", "127.0.0.1")

	v, err := parseBindAddr()
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", v)
}

func TestParseBindAddrValidIPv6(t *testing.T) {
	t.Setenv("SL_BIND_ADDR", "::1")

	v, err := parseBindAddr()
	require.NoError(t, err)
	assert.Equal(t, "::1", v)
}

func TestParseBindAddrInvalid(t *testing.T) {
	t.Setenv("SL_BIND_ADDR", "not-an-ip")

	_, err := parseBindAddr()
	assert.Error(t, err, "expected error for invalid IP address")
}

func TestGetEnvRequestLogFileSet(t *testing.T) {
	clearStreamEnv(t)
	t.Setenv("SL_STREAM_1_TOKEN", "abcdefghijklmnopqrstuvwxyz123456")
	t.Setenv("SL_REQUEST_LOG_FILE", "/tmp/requests.log")

	env, err := GetEnv()
	require.NoError(t, err)
	assert.Equal(t, "/tmp/requests.log", env.REQUEST_LOG_FILE)
}

func TestGetEnvRequestLogFileUnset(t *testing.T) {
	clearStreamEnv(t)
	t.Setenv("SL_STREAM_1_TOKEN", "abcdefghijklmnopqrstuvwxyz123456")
	os.Unsetenv("SL_REQUEST_LOG_FILE")

	env, err := GetEnv()
	require.NoError(t, err)
	assert.Empty(t, env.REQUEST_LOG_FILE)
}

func TestParseStreamIdleTimeoutDefault(t *testing.T) {
	t.Setenv("SL_STREAM_IDLE_TIMEOUT", "")

	v, err := parseStreamIdleTimeout()
	require.NoError(t, err)
	assert.Equal(t, DefaultStreamIdleTimeout, v)
}

func TestParseStreamIdleTimeoutCustom(t *testing.T) {
	t.Setenv("SL_STREAM_IDLE_TIMEOUT", "90s")

	v, err := parseStreamIdleTimeout()
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, v)
}

func TestParseStreamIdleTimeoutZeroDisables(t *testing.T) {
	t.Setenv("SL_STREAM_IDLE_TIMEOUT", "0")

	// "0" is not a valid Go duration; "0s" is. But time.ParseDuration accepts "0".
	v, err := parseStreamIdleTimeout()
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), v)
}

func TestParseStreamIdleTimeoutNegativeRejected(t *testing.T) {
	t.Setenv("SL_STREAM_IDLE_TIMEOUT", "-5s")

	_, err := parseStreamIdleTimeout()
	assert.Error(t, err, "expected error for negative value")
}

func TestParseStreamIdleTimeoutInvalid(t *testing.T) {
	t.Setenv("SL_STREAM_IDLE_TIMEOUT", "not-a-duration")

	_, err := parseStreamIdleTimeout()
	assert.Error(t, err, "expected error for invalid format")
}
