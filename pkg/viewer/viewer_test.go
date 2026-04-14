package viewer

import (
	"bytes"
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

func testEnvKey() []byte {
	return []byte("0123456789abcdef0123456789abcdef")
}

// testKey returns a DeriveKey output suitable for tests that don't care
// about the (stream, type) pairing — just a valid derived key.
func testKey(t *testing.T) []byte {
	t.Helper()
	k, err := DeriveKey(testEnvKey(), "s1", TypePlaylist)
	require.NoError(t, err)
	return k
}

func TestMintAndVerifyRoundTrip(t *testing.T) {
	key := testKey(t)
	now := time.UnixMilli(1_700_000_000_000)
	exp := now.Add(1 * time.Hour).UnixMilli()

	tok, err := Mint(key, exp)
	require.NoError(t, err)
	assert.Len(t, tok, EncodedTokenLen)

	err = Verify(key, now, tok)
	require.NoError(t, err)
}

// TestDeriveKey_Deterministic asserts that identical inputs produce
// identical derived keys — the KDF is a pure function of its inputs.
func TestDeriveKey_Deterministic(t *testing.T) {
	a, err := DeriveKey(testEnvKey(), "s1", TypePlaylist)
	require.NoError(t, err)
	b, err := DeriveKey(testEnvKey(), "s1", TypePlaylist)
	require.NoError(t, err)
	assert.Equal(t, a, b)
}

// TestDeriveKey_DomainSeparation asserts that varying any KDF input
// (envKey, streamID, type) produces a distinct derived key. This is the
// security-relevant property underpinning cross-stream and cross-type
// isolation.
func TestDeriveKey_DomainSeparation(t *testing.T) {
	base, err := DeriveKey(testEnvKey(), "s1", TypePlaylist)
	require.NoError(t, err)

	// Different env key.
	other, err := DeriveKey([]byte("ffffffffffffffffffffffffffffffff"), "s1", TypePlaylist)
	require.NoError(t, err)
	assert.NotEqual(t, base, other, "different env key must derive distinct key")

	// Different stream ID.
	other, err = DeriveKey(testEnvKey(), "s2", TypePlaylist)
	require.NoError(t, err)
	assert.NotEqual(t, base, other, "different streamID must derive distinct key")

	// Different type.
	other, err = DeriveKey(testEnvKey(), "s1", TypeSegment)
	require.NoError(t, err)
	assert.NotEqual(t, base, other, "different type must derive distinct key")
}

// TestDeriveKey_EmptyEnvKey asserts the error sentinel for an empty secret.
func TestDeriveKey_EmptyEnvKey(t *testing.T) {
	_, err := DeriveKey(nil, "s1", TypePlaylist)
	assert.ErrorIs(t, err, ErrEmptyKey)
	_, err = DeriveKey([]byte{}, "s1", TypePlaylist)
	assert.ErrorIs(t, err, ErrEmptyKey)
}

// TestDeriveKey_UnknownType asserts the error sentinel for an unrecognized
// type. This is a defense against programmer error — no external input can
// reach DeriveKey with an unknown Type.
func TestDeriveKey_UnknownType(t *testing.T) {
	_, err := DeriveKey(testEnvKey(), "s1", Type(0))
	assert.ErrorIs(t, err, ErrUnknownType)
	_, err = DeriveKey(testEnvKey(), "s1", Type(99))
	assert.ErrorIs(t, err, ErrUnknownType)
}

// TestVerify_CrossTypeKeyRejected asserts that a token minted under one
// type's derived key fails Verify under the other type's derived key —
// even with the same env secret and stream ID. This replaces the old
// type-byte tamper test: type scoping is now enforced by the KDF rather
// than by a MAC-covered payload byte, and the property still holds.
func TestVerify_CrossTypeKeyRejected(t *testing.T) {
	playlistKey, err := DeriveKey(testEnvKey(), "s1", TypePlaylist)
	require.NoError(t, err)
	segmentKey, err := DeriveKey(testEnvKey(), "s1", TypeSegment)
	require.NoError(t, err)

	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(segmentKey, now.Add(time.Hour).UnixMilli())
	require.NoError(t, err)

	err = Verify(playlistKey, now, tok)
	assert.ErrorIs(t, err, ErrBadMAC, "segment-class token must fail under playlist-derived key")

	// Sanity: it does verify under its own key.
	err = Verify(segmentKey, now, tok)
	assert.NoError(t, err)
}

// TestVerify_CrossStreamKeyRejected asserts that a token minted under one
// stream's derived key fails Verify under another stream's derived key.
// This is the KDF-backed cross-stream binding — no longer an implicit
// property of distinct env-var values.
func TestVerify_CrossStreamKeyRejected(t *testing.T) {
	// Same env secret across two stream IDs — KDF must still isolate.
	keyA, err := DeriveKey(testEnvKey(), "sA", TypePlaylist)
	require.NoError(t, err)
	keyB, err := DeriveKey(testEnvKey(), "sB", TypePlaylist)
	require.NoError(t, err)

	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(keyA, now.Add(time.Hour).UnixMilli())
	require.NoError(t, err)

	err = Verify(keyB, now, tok)
	assert.ErrorIs(t, err, ErrBadMAC, "token minted for stream A must not verify for stream B")
}

func TestMintEmptyKey(t *testing.T) {
	_, err := Mint(nil, 1)
	assert.ErrorIs(t, err, ErrEmptyKey)
}

func TestVerifyEmptyKey(t *testing.T) {
	tok, err := Mint(testKey(t), time.UnixMilli(1_700_000_000_000).Add(time.Hour).UnixMilli())
	require.NoError(t, err)
	err = Verify(nil, time.UnixMilli(1_700_000_000_000), tok)
	assert.ErrorIs(t, err, ErrEmptyKey)
}

// TestMintRoundsDownToMinute asserts that sub-minute resolution is lost during
// encoding. The effective encoded expiry is floor(expiresAtMs / 60_000) * 60_000.
func TestMintRoundsDownToMinute(t *testing.T) {
	key := testKey(t)
	// Two inputs whose floor-minute is identical must encode identically.
	a, err := Mint(key, 90_000) // 1.5 minutes
	require.NoError(t, err)
	b, err := Mint(key, 60_000) // 1.0 minutes (same floor-minute)
	require.NoError(t, err)
	assert.Equal(t, a, b, "sub-minute resolution must be discarded at encode")

	// Decoding the raw payload should yield exactly 1 minute.
	raw, err := base64.RawURLEncoding.DecodeString(a)
	require.NoError(t, err)
	require.Len(t, raw, TokenBytes)
	got := binary.BigEndian.Uint32(raw[1:headerLen])
	assert.Equal(t, uint32(1), got)
}

func TestMintRejectsNegativeExpiry(t *testing.T) {
	_, err := Mint(testKey(t), -1)
	assert.ErrorIs(t, err, ErrMalformed)
}

func TestMintRejectsOverflow(t *testing.T) {
	// One minute past the uint32 range.
	_, err := Mint(testKey(t), int64(math.MaxUint32+1)*msPerMinute)
	assert.ErrorIs(t, err, ErrMalformed)
}

func TestVerifyExpiredExact(t *testing.T) {
	key := testKey(t)
	// Use a minute-aligned "now" so the round-trip is exact.
	now := time.UnixMilli(60_000 * 28_333_333)
	tok, err := Mint(key, now.UnixMilli())
	require.NoError(t, err)
	// Equal-to-now must be treated as expired.
	err = Verify(key, now, tok)
	assert.ErrorIs(t, err, ErrExpired)
}

func TestVerifyExpiredPast(t *testing.T) {
	key := testKey(t)
	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(key, now.Add(-1*time.Minute).UnixMilli())
	require.NoError(t, err)
	err = Verify(key, now, tok)
	assert.ErrorIs(t, err, ErrExpired)
}

// TestVerifyValidOneMinuteAfterNow exercises the smallest positive lifetime
// representable at minute precision: an expiry one full minute beyond now's
// minute boundary.
func TestVerifyValidOneMinuteAfterNow(t *testing.T) {
	key := testKey(t)
	// Minute-aligned now.
	now := time.UnixMilli(60_000 * 28_333_333)
	tok, err := Mint(key, now.Add(1*time.Minute).UnixMilli())
	require.NoError(t, err)
	err = Verify(key, now, tok)
	assert.NoError(t, err)
}

func TestVerifyWrongKey(t *testing.T) {
	key := testKey(t)
	otherKey, err := DeriveKey([]byte("ffffffffffffffffffffffffffffffff"), "s1", TypePlaylist)
	require.NoError(t, err)
	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(key, now.Add(time.Hour).UnixMilli())
	require.NoError(t, err)
	err = Verify(otherKey, now, tok)
	assert.ErrorIs(t, err, ErrBadMAC)
}

func TestVerifyTamperedMACByte(t *testing.T) {
	key := testKey(t)
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
	key := testKey(t)
	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(key, now.Add(time.Hour).UnixMilli())
	require.NoError(t, err)

	raw, err := base64.RawURLEncoding.DecodeString(tok)
	require.NoError(t, err)
	raw[2] ^= 0x01 // flip a byte inside the 4-byte minutes field (indices 1..4)
	bad := base64.RawURLEncoding.EncodeToString(raw)

	err = Verify(key, now, bad)
	// Flipping an exp byte invalidates the MAC.
	assert.ErrorIs(t, err, ErrBadMAC)
}

func TestVerifyTamperedVersionByte(t *testing.T) {
	key := testKey(t)
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
	key := testKey(t)
	now := time.UnixMilli(1_700_000_000_000)

	var buf [TokenBytes]byte
	buf[0] = 2
	expMinutes := uint32(now.Add(time.Hour).UnixMilli() / msPerMinute)
	binary.BigEndian.PutUint32(buf[1:headerLen], expMinutes)
	mac := computeMAC(key, buf[:headerLen])
	copy(buf[headerLen:], mac)

	tok := base64.RawURLEncoding.EncodeToString(buf[:])
	err := Verify(key, now, tok)
	assert.ErrorIs(t, err, ErrUnsupportedVersion)
}

func TestVerifyMalformedBase64(t *testing.T) {
	err := Verify(testKey(t), time.UnixMilli(1_700_000_000_000), "not-a-valid-token!!!!")
	assert.ErrorIs(t, err, ErrMalformed)
}

// TestVerifyRejectsFinalCharMutation asserts that any mutation of the
// token's final base64url character is rejected. At TokenBytes=21 the
// encoding is a 1:1 mapping (168 payload bits = 28 chars × 6 bits, no
// unused trailing bits), so every distinct final-char value decodes to a
// distinct payload — which then fails the MAC check. This test doubles
// as a regression guard: if TokenBytes ever becomes a non-multiple of 3,
// some final-char variants would decode to the same bytes with differing
// unused bits, and Strict() base64 decoding would reject them as
// ErrMalformed. Either outcome (ErrBadMAC today, ErrMalformed under a
// future payload size) is a valid rejection.
func TestVerifyRejectsFinalCharMutation(t *testing.T) {
	key := testKey(t)
	now := time.UnixMilli(1_700_000_000_000)
	tok, err := Mint(key, now.Add(time.Hour).UnixMilli())
	require.NoError(t, err)
	require.Len(t, tok, EncodedTokenLen)

	// Index of the base64url alphabet for a char.
	alphaIdx := func(c byte) int {
		switch {
		case c >= 'A' && c <= 'Z':
			return int(c - 'A')
		case c >= 'a' && c <= 'z':
			return int(c-'a') + 26
		case c >= '0' && c <= '9':
			return int(c-'0') + 52
		case c == '-':
			return 62
		case c == '_':
			return 63
		}
		t.Fatalf("unexpected base64url char %q", c)
		return -1
	}
	alphaChar := func(i int) byte {
		const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
		return alpha[i]
	}

	last := tok[EncodedTokenLen-1]
	origIdx := alphaIdx(last)
	variants := 0
	for flip := 1; flip < 64; flip++ {
		idx := origIdx ^ flip
		if idx < 0 || idx > 63 || idx == origIdx {
			continue
		}
		bad := tok[:EncodedTokenLen-1] + string(alphaChar(idx))
		if bad == tok {
			continue
		}
		verr := Verify(key, now, bad)
		assert.Truef(t,
			errors.Is(verr, ErrMalformed) || errors.Is(verr, ErrBadMAC),
			"mutated final char (flip=%d) must be rejected; got %v", flip, verr)
		variants++
	}
	require.Greater(t, variants, 0, "expected at least one variant to test")
}

func TestVerifyMalformedWrongLength(t *testing.T) {
	short := base64.RawURLEncoding.EncodeToString(make([]byte, TokenBytes-1))
	err := Verify(testKey(t), time.UnixMilli(1_700_000_000_000), short)
	assert.ErrorIs(t, err, ErrMalformed)

	long := base64.RawURLEncoding.EncodeToString(make([]byte, TokenBytes+1))
	err = Verify(testKey(t), time.UnixMilli(1_700_000_000_000), long)
	assert.ErrorIs(t, err, ErrMalformed)
}

// TestVerifyRejectsOldWireFormat asserts that a v1 22-byte token (old
// format with a type byte) is rejected as malformed — it has the wrong
// length. This protects against accidental roll-forward of cached tokens
// across a deploy where the binary changed formats.
func TestVerifyRejectsOldWireFormat(t *testing.T) {
	// Hand-assemble a 22-byte payload shaped like the old format and
	// confirm it fails the length check.
	oldBuf := make([]byte, 22)
	oldBuf[0] = Version
	oldBuf[1] = byte(TypePlaylist)
	tok := base64.RawURLEncoding.EncodeToString(oldBuf)
	err := Verify(testKey(t), time.UnixMilli(1_700_000_000_000), tok)
	assert.ErrorIs(t, err, ErrMalformed)
}

func TestVerifyEmptyToken(t *testing.T) {
	err := Verify(testKey(t), time.UnixMilli(1_700_000_000_000), "")
	assert.ErrorIs(t, err, ErrMalformed)
}

func TestTokenIsURLSafe(t *testing.T) {
	key := testKey(t)
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
	key := testKey(t)
	exp := int64(1_700_000_000_000)
	a, err := Mint(key, exp)
	require.NoError(t, err)
	b, err := Mint(key, exp)
	require.NoError(t, err)
	assert.Equal(t, a, b)
}

// TestDeriveKey_NotEqualEnvKey asserts the derived key is not the raw env
// key — a hygiene regression guard against any future refactor that
// accidentally returns the input bytes.
func TestDeriveKey_NotEqualEnvKey(t *testing.T) {
	k, err := DeriveKey(testEnvKey(), "s1", TypePlaylist)
	require.NoError(t, err)
	assert.False(t, bytes.Equal(k, testEnvKey()))
}

func TestErrorSentinelsDistinct(t *testing.T) {
	// Guard against accidental sentinel aliasing.
	sentinels := []error{ErrMalformed, ErrUnsupportedVersion, ErrBadMAC, ErrExpired, ErrEmptyKey, ErrUnknownType}
	for i := range sentinels {
		for j := range sentinels {
			if i == j {
				continue
			}
			assert.False(t, errors.Is(sentinels[i], sentinels[j]), "%v should not match %v", sentinels[i], sentinels[j])
		}
	}
}
