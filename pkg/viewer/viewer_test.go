package viewer

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math"
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

	tok, err := Mint(key, exp, TypeViewer)
	require.NoError(t, err)
	assert.Len(t, tok, EncodedTokenLen)

	typ, err := Verify(key, now, tok)
	require.NoError(t, err)
	assert.Equal(t, TypeViewer, typ)
}

// TestMintAndVerifyPreservesType asserts the Type round-trips faithfully;
// the middleware relies on this to distinguish viewer tokens from
// playlist-scoped segment tokens.
func TestMintAndVerifyPreservesType(t *testing.T) {
	key := testKey()
	now := time.UnixMilli(1_700_000_000_000)
	exp := now.Add(time.Hour).UnixMilli()

	for _, want := range []Type{TypeViewer, TypeSegment} {
		tok, err := Mint(key, exp, want)
		require.NoError(t, err)
		got, err := Verify(key, now, tok)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
}

// TestMintDistinctTypesProduceDistinctTokens asserts that two tokens with
// identical expiry but different types are not interchangeable — the type
// byte is part of the MAC input.
func TestMintDistinctTypesProduceDistinctTokens(t *testing.T) {
	key := testKey()
	exp := int64(1_700_000_000_000)
	a, err := Mint(key, exp, TypeViewer)
	require.NoError(t, err)
	b, err := Mint(key, exp, TypeSegment)
	require.NoError(t, err)
	assert.NotEqual(t, a, b, "distinct types must produce distinct tokens")
}

// TestVerifyTamperedTypeByte asserts that flipping the type byte in a valid
// token invalidates the MAC. Without this coverage an attacker could
// upgrade a TypeSegment token to TypeViewer by bit-flipping the payload.
func TestVerifyTamperedTypeByte(t *testing.T) {
	key := testKey()
	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(key, now.Add(time.Hour).UnixMilli(), TypeSegment)
	require.NoError(t, err)

	raw, err := base64.RawURLEncoding.DecodeString(tok)
	require.NoError(t, err)
	raw[1] = byte(TypeViewer) // try to upgrade
	bad := base64.RawURLEncoding.EncodeToString(raw)

	_, err = Verify(key, now, bad)
	assert.ErrorIs(t, err, ErrBadMAC)
}

func TestMintEmptyKey(t *testing.T) {
	_, err := Mint(nil, 1, TypeViewer)
	assert.ErrorIs(t, err, ErrEmptyKey)
}

func TestVerifyEmptyKey(t *testing.T) {
	tok, err := Mint(testKey(), time.UnixMilli(1_700_000_000_000).Add(time.Hour).UnixMilli(), TypeViewer)
	require.NoError(t, err)
	_, err = Verify(nil, time.UnixMilli(1_700_000_000_000), tok)
	assert.ErrorIs(t, err, ErrEmptyKey)
}

// TestMintRoundsDownToMinute asserts that sub-minute resolution is lost during
// encoding. The effective encoded expiry is floor(expiresAtMs / 60_000) * 60_000.
func TestMintRoundsDownToMinute(t *testing.T) {
	key := testKey()
	// Two inputs whose floor-minute is identical must encode identically.
	a, err := Mint(key, 90_000, TypeViewer) // 1.5 minutes
	require.NoError(t, err)
	b, err := Mint(key, 60_000, TypeViewer) // 1.0 minutes (same floor-minute)
	require.NoError(t, err)
	assert.Equal(t, a, b, "sub-minute resolution must be discarded at encode")

	// Decoding the raw payload should yield exactly 1 minute.
	raw, err := base64.RawURLEncoding.DecodeString(a)
	require.NoError(t, err)
	require.Len(t, raw, TokenBytes)
	got := binary.BigEndian.Uint32(raw[2:headerLen])
	assert.Equal(t, uint32(1), got)
}

func TestMintRejectsNegativeExpiry(t *testing.T) {
	_, err := Mint(testKey(), -1, TypeViewer)
	assert.ErrorIs(t, err, ErrMalformed)
}

func TestMintRejectsOverflow(t *testing.T) {
	// One minute past the uint32 range.
	_, err := Mint(testKey(), int64(math.MaxUint32+1)*msPerMinute, TypeViewer)
	assert.ErrorIs(t, err, ErrMalformed)
}

func TestVerifyExpiredExact(t *testing.T) {
	key := testKey()
	// Use a minute-aligned "now" so the round-trip is exact.
	now := time.UnixMilli(60_000 * 28_333_333)
	tok, err := Mint(key, now.UnixMilli(), TypeViewer)
	require.NoError(t, err)
	// Equal-to-now must be treated as expired.
	_, err = Verify(key, now, tok)
	assert.ErrorIs(t, err, ErrExpired)
}

func TestVerifyExpiredPast(t *testing.T) {
	key := testKey()
	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(key, now.Add(-1*time.Minute).UnixMilli(), TypeViewer)
	require.NoError(t, err)
	_, err = Verify(key, now, tok)
	assert.ErrorIs(t, err, ErrExpired)
}

// TestVerifyValidOneMinuteAfterNow exercises the smallest positive lifetime
// representable at minute precision: an expiry one full minute beyond now's
// minute boundary.
func TestVerifyValidOneMinuteAfterNow(t *testing.T) {
	key := testKey()
	// Minute-aligned now.
	now := time.UnixMilli(60_000 * 28_333_333)
	tok, err := Mint(key, now.Add(1*time.Minute).UnixMilli(), TypeViewer)
	require.NoError(t, err)
	_, err = Verify(key, now, tok)
	assert.NoError(t, err)
}

func TestVerifyWrongKey(t *testing.T) {
	key := testKey()
	otherKey := []byte("ffffffffffffffffffffffffffffffff")
	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(key, now.Add(time.Hour).UnixMilli(), TypeViewer)
	require.NoError(t, err)
	_, err = Verify(otherKey, now, tok)
	assert.ErrorIs(t, err, ErrBadMAC)
}

func TestVerifyTamperedMACByte(t *testing.T) {
	key := testKey()
	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(key, now.Add(time.Hour).UnixMilli(), TypeViewer)
	require.NoError(t, err)

	raw, err := base64.RawURLEncoding.DecodeString(tok)
	require.NoError(t, err)
	raw[TokenBytes-1] ^= 0x01
	bad := base64.RawURLEncoding.EncodeToString(raw)

	_, err = Verify(key, now, bad)
	assert.ErrorIs(t, err, ErrBadMAC)
}

func TestVerifyTamperedExpByte(t *testing.T) {
	key := testKey()
	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(key, now.Add(time.Hour).UnixMilli(), TypeViewer)
	require.NoError(t, err)

	raw, err := base64.RawURLEncoding.DecodeString(tok)
	require.NoError(t, err)
	raw[3] ^= 0x01 // flip a byte inside the 4-byte minutes field (indices 2..5)
	bad := base64.RawURLEncoding.EncodeToString(raw)

	_, err = Verify(key, now, bad)
	// Flipping an exp byte invalidates the MAC.
	assert.ErrorIs(t, err, ErrBadMAC)
}

func TestVerifyTamperedVersionByte(t *testing.T) {
	key := testKey()
	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(key, now.Add(time.Hour).UnixMilli(), TypeViewer)
	require.NoError(t, err)

	raw, err := base64.RawURLEncoding.DecodeString(tok)
	require.NoError(t, err)
	raw[0] = 2 // unsupported version
	bad := base64.RawURLEncoding.EncodeToString(raw)

	_, err = Verify(key, now, bad)
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
	buf[1] = byte(TypeViewer)
	expMinutes := uint32(now.Add(time.Hour).UnixMilli() / msPerMinute)
	binary.BigEndian.PutUint32(buf[2:headerLen], expMinutes)
	mac := computeMAC(key, buf[:headerLen])
	copy(buf[headerLen:], mac)

	tok := base64.RawURLEncoding.EncodeToString(buf[:])
	_, err := Verify(key, now, tok)
	assert.ErrorIs(t, err, ErrUnsupportedVersion)
}

func TestVerifyMalformedBase64(t *testing.T) {
	_, err := Verify(testKey(), time.UnixMilli(1_700_000_000_000), "not-a-valid-token!!!!")
	assert.ErrorIs(t, err, ErrMalformed)
}

func TestVerifyMalformedWrongLength(t *testing.T) {
	short := base64.RawURLEncoding.EncodeToString(make([]byte, TokenBytes-1))
	_, err := Verify(testKey(), time.UnixMilli(1_700_000_000_000), short)
	assert.ErrorIs(t, err, ErrMalformed)

	long := base64.RawURLEncoding.EncodeToString(make([]byte, TokenBytes+1))
	_, err = Verify(testKey(), time.UnixMilli(1_700_000_000_000), long)
	assert.ErrorIs(t, err, ErrMalformed)
}

func TestVerifyEmptyToken(t *testing.T) {
	_, err := Verify(testKey(), time.UnixMilli(1_700_000_000_000), "")
	assert.ErrorIs(t, err, ErrMalformed)
}

func TestTokenIsURLSafe(t *testing.T) {
	key := testKey()
	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(key, now.Add(time.Hour).UnixMilli(), TypeViewer)
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
	a, err := Mint(key, exp, TypeViewer)
	require.NoError(t, err)
	b, err := Mint(key, exp, TypeViewer)
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
