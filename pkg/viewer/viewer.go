// Package viewer provides stateless viewer tokens for HLS stream playback.
//
// A token is a compact, URL-safe binary blob consisting of:
//
//	[1 byte version][4 bytes uint32 big-endian unix minutes][16 bytes truncated HMAC-SHA256]
//
// The MAC covers the version byte and the expiration bytes, keyed by a
// per-stream secret. Tokens are encoded with base64.RawURLEncoding, yielding
// a fixed 28-character string. Tokens are not bound to a stream ID; isolation
// across streams is provided by using a distinct signing key per stream.
//
// The expiration is encoded with 1-minute precision: the ms-granularity value
// passed to Mint is truncated to the preceding minute boundary. Callers that
// need to know the exact encoded expiry should floor their input to a minute
// beforehand (Mint does this internally regardless). The range of a uint32
// minute counter reaches beyond year 10000, so practical overflow is
// impossible.
package viewer

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math"
	"time"
)

// Version is the current token wire-format version.
const Version byte = 1

// macLen is the truncated HMAC length in bytes (128-bit).
const macLen = 16

// TokenBytes is the length of a decoded token in bytes:
// version (1) + uint32 minutes (4) + MAC (16) = 21.
const TokenBytes = 1 + 4 + macLen

// EncodedTokenLen is the length of a base64url-encoded (no padding) token.
const EncodedTokenLen = 28

// msPerMinute is the resolution of the encoded expiration field. Kept
// package-private because the minute unit does not leak beyond serde.
const msPerMinute = 60_000

// Sentinel errors returned by Verify. HTTP handlers should collapse all of
// these to a single 401 response; the distinction exists only for logging.
var (
	ErrMalformed          = errors.New("viewer: malformed token")
	ErrUnsupportedVersion = errors.New("viewer: unsupported token version")
	ErrBadMAC             = errors.New("viewer: bad MAC")
	ErrExpired            = errors.New("viewer: token expired")
	ErrEmptyKey           = errors.New("viewer: signing key must not be empty")
)

// Mint produces a token for the given expiration (unix ms). The expiry is
// internally floored to the preceding minute boundary before encoding; see
// the package comment for rationale. The key must be non-empty; callers are
// expected to enforce a minimum length upstream.
//
// Returns ErrMalformed if the minute-aligned expiration is negative or does
// not fit in a uint32 (overflow after year ~10140).
func Mint(key []byte, expiresAtMs int64) (string, error) {
	if len(key) == 0 {
		return "", ErrEmptyKey
	}
	if expiresAtMs < 0 {
		return "", ErrMalformed
	}
	expMinutes := expiresAtMs / msPerMinute
	if expMinutes > math.MaxUint32 {
		return "", ErrMalformed
	}

	var buf [TokenBytes]byte
	buf[0] = Version
	binary.BigEndian.PutUint32(buf[1:5], uint32(expMinutes))

	mac := computeMAC(key, buf[:5])
	copy(buf[5:], mac)

	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// Verify decodes token and checks its MAC and expiration against now.
// Returns nil on success, or one of the sentinel errors above. Timing is
// kept uniform across MAC failures by always running the HMAC compare;
// malformed inputs are compared against a zeroed canonical payload so the
// MAC path still executes.
func Verify(key []byte, now time.Time, token string) error {
	if len(key) == 0 {
		return ErrEmptyKey
	}

	var payload [TokenBytes]byte
	malformed := false

	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != TokenBytes {
		// Compare the zeroed payload against itself-keyed MAC to keep
		// timing uniform. The equality check below will still fail via
		// the malformed flag.
		malformed = true
	} else {
		copy(payload[:], raw)
	}

	expected := computeMAC(key, payload[:5])
	if !hmac.Equal(expected, payload[5:]) || malformed {
		if malformed {
			return ErrMalformed
		}
		return ErrBadMAC
	}

	if payload[0] != Version {
		return ErrUnsupportedVersion
	}

	expMinutes := binary.BigEndian.Uint32(payload[1:5])
	expMs := int64(expMinutes) * msPerMinute
	if expMs <= now.UnixMilli() {
		return ErrExpired
	}

	return nil
}

// computeMAC returns the truncated HMAC-SHA256 of msg under key.
func computeMAC(key, msg []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	sum := h.Sum(nil)
	return sum[:macLen]
}
