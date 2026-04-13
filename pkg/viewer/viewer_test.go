package viewer

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKey() []byte {
	return []byte("0123456789abcdef0123456789abcdef")
}

func TestMintAndVerifyRoundTrip(t *testing.T) {
	key := testKey()
	now := time.UnixMilli(1_700_000_000_000)
	exp := now.Add(1 * time.Hour).UnixMilli()

	tok, err := Mint(key, exp)
	require.NoError(t, err)
	assert.Len(t, tok, EncodedTokenLen)

	err = Verify(key, now, tok)
	assert.NoError(t, err)
}

func TestMintEmptyKey(t *testing.T) {
	_, err := Mint(nil, 1)
	assert.ErrorIs(t, err, ErrEmptyKey)
}

func TestVerifyEmptyKey(t *testing.T) {
	tok, err := Mint(testKey(), time.UnixMilli(1_700_000_000_000).Add(time.Hour).UnixMilli())
	require.NoError(t, err)
	err = Verify(nil, time.UnixMilli(1_700_000_000_000), tok)
	assert.ErrorIs(t, err, ErrEmptyKey)
}

func TestVerifyExpiredExact(t *testing.T) {
	key := testKey()
	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(key, now.UnixMilli())
	require.NoError(t, err)
	// Equal-to-now must be treated as expired.
	err = Verify(key, now, tok)
	assert.ErrorIs(t, err, ErrExpired)
}

func TestVerifyExpiredPast(t *testing.T) {
	key := testKey()
	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(key, now.Add(-1*time.Millisecond).UnixMilli())
	require.NoError(t, err)
	err = Verify(key, now, tok)
	assert.ErrorIs(t, err, ErrExpired)
}

func TestVerifyValidOneMsBeforeExpiry(t *testing.T) {
	key := testKey()
	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(key, now.Add(1*time.Millisecond).UnixMilli())
	require.NoError(t, err)
	err = Verify(key, now, tok)
	assert.NoError(t, err)
}

func TestVerifyWrongKey(t *testing.T) {
	key := testKey()
	otherKey := []byte("ffffffffffffffffffffffffffffffff")
	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(key, now.Add(time.Hour).UnixMilli())
	require.NoError(t, err)
	err = Verify(otherKey, now, tok)
	assert.ErrorIs(t, err, ErrBadMAC)
}

func TestVerifyTamperedMACByte(t *testing.T) {
	key := testKey()
	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(key, now.Add(time.Hour).UnixMilli())
	require.NoError(t, err)

	raw, err := base64.RawURLEncoding.DecodeString(tok)
	require.NoError(t, err)
	raw[TokenBytes-1] ^= 0x01
	bad := base64.RawURLEncoding.EncodeToString(raw)

	err = Verify(key, now, bad)
	assert.ErrorIs(t, err, ErrBadMAC)
}

func TestVerifyTamperedExpByte(t *testing.T) {
	key := testKey()
	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(key, now.Add(time.Hour).UnixMilli())
	require.NoError(t, err)

	raw, err := base64.RawURLEncoding.DecodeString(tok)
	require.NoError(t, err)
	raw[5] ^= 0x01 // flip a byte inside the exp_ms field
	bad := base64.RawURLEncoding.EncodeToString(raw)

	err = Verify(key, now, bad)
	// Flipping an exp byte invalidates the MAC.
	assert.ErrorIs(t, err, ErrBadMAC)
}

func TestVerifyTamperedVersionByte(t *testing.T) {
	key := testKey()
	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(key, now.Add(time.Hour).UnixMilli())
	require.NoError(t, err)

	raw, err := base64.RawURLEncoding.DecodeString(tok)
	require.NoError(t, err)
	raw[0] = 2 // unsupported version
	bad := base64.RawURLEncoding.EncodeToString(raw)

	err = Verify(key, now, bad)
	// The MAC covers the version byte, so this fails MAC check, not version check.
	assert.ErrorIs(t, err, ErrBadMAC)
}

func TestVerifyUnsupportedVersionWithValidMAC(t *testing.T) {
	// Directly forge a payload with version=2 and a valid MAC under the same key,
	// to prove the version check triggers when MAC is good.
	key := testKey()
	now := time.UnixMilli(1_700_000_000_000)

	var buf [TokenBytes]byte
	buf[0] = 2
	expMs := now.Add(time.Hour).UnixMilli()
	for i := 0; i < 8; i++ {
		buf[1+i] = byte(expMs >> (56 - 8*i))
	}
	mac := computeMAC(key, buf[:9])
	copy(buf[9:], mac)

	tok := base64.RawURLEncoding.EncodeToString(buf[:])
	err := Verify(key, now, tok)
	assert.ErrorIs(t, err, ErrUnsupportedVersion)
}

func TestVerifyMalformedBase64(t *testing.T) {
	err := Verify(testKey(), time.UnixMilli(1_700_000_000_000), "not-a-valid-token!!!!")
	assert.ErrorIs(t, err, ErrMalformed)
}

func TestVerifyMalformedWrongLength(t *testing.T) {
	// 24-byte payload encoded (one byte short).
	short := base64.RawURLEncoding.EncodeToString(make([]byte, TokenBytes-1))
	err := Verify(testKey(), time.UnixMilli(1_700_000_000_000), short)
	assert.ErrorIs(t, err, ErrMalformed)

	long := base64.RawURLEncoding.EncodeToString(make([]byte, TokenBytes+1))
	err = Verify(testKey(), time.UnixMilli(1_700_000_000_000), long)
	assert.ErrorIs(t, err, ErrMalformed)
}

func TestVerifyEmptyToken(t *testing.T) {
	err := Verify(testKey(), time.UnixMilli(1_700_000_000_000), "")
	assert.ErrorIs(t, err, ErrMalformed)
}

func TestTokenIsURLSafe(t *testing.T) {
	key := testKey()
	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(key, now.Add(time.Hour).UnixMilli())
	require.NoError(t, err)
	// base64url alphabet: A-Z, a-z, 0-9, '-', '_'. No padding '='.
	assert.False(t, strings.ContainsAny(tok, "+/="))
	for _, c := range tok {
		isAlphaNum := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		isURLSafe := c == '-' || c == '_'
		assert.True(t, isAlphaNum || isURLSafe, "unexpected char %q in token", c)
	}
}

func TestMintDeterministic(t *testing.T) {
	// Minting with the same inputs must produce the same token — no randomness.
	key := testKey()
	exp := int64(1_700_000_000_000)
	a, err := Mint(key, exp)
	require.NoError(t, err)
	b, err := Mint(key, exp)
	require.NoError(t, err)
	assert.Equal(t, a, b)
}

func TestErrorSentinelsDistinct(t *testing.T) {
	// Guard against accidental sentinel aliasing.
	sentinels := []error{ErrMalformed, ErrUnsupportedVersion, ErrBadMAC, ErrExpired, ErrEmptyKey}
	for i := range sentinels {
		for j := range sentinels {
			if i == j {
				continue
			}
			assert.False(t, errors.Is(sentinels[i], sentinels[j]), "%v should not match %v", sentinels[i], sentinels[j])
		}
	}
}
