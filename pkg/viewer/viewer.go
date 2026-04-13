// Package viewer provides stateless viewer tokens for HLS stream playback.
//
// A token is a compact, URL-safe binary blob consisting of:
//
//	[1 byte version][8 bytes int64 big-endian exp_ms][16 bytes truncated HMAC-SHA256]
//
// The MAC covers the version byte and the expiration bytes, keyed by a
// per-stream secret. Tokens are encoded with base64.RawURLEncoding, yielding
// a fixed 34-character string. Tokens are not bound to a stream ID; isolation
// across streams is provided by using a distinct signing key per stream.
package viewer

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"time"
)

// Version is the current token wire-format version.
const Version byte = 1

// macLen is the truncated HMAC length in bytes (128-bit).
const macLen = 16

// TokenBytes is the length of a decoded token in bytes.
const TokenBytes = 1 + 8 + macLen

// EncodedTokenLen is the length of a base64url-encoded (no padding) token.
const EncodedTokenLen = 34

// Sentinel errors returned by Verify. HTTP handlers should collapse all of
// these to a single 401 response; the distinction exists only for logging.
var (
	ErrMalformed          = errors.New("viewer: malformed token")
	ErrUnsupportedVersion = errors.New("viewer: unsupported token version")
	ErrBadMAC             = errors.New("viewer: bad MAC")
	ErrExpired            = errors.New("viewer: token expired")
	ErrEmptyKey           = errors.New("viewer: signing key must not be empty")
)

// Mint produces a token for the given expiration (unix ms). The key must be
// non-empty; callers are expected to enforce a minimum length upstream.
func Mint(key []byte, expiresAtMs int64) (string, error) {
	if len(key) == 0 {
		return "", ErrEmptyKey
	}
	var buf [TokenBytes]byte
	buf[0] = Version
	binary.BigEndian.PutUint64(buf[1:9], uint64(expiresAtMs)) // #nosec G115 -- expiresAtMs is reinterpreted as an unsigned bit pattern for fixed-size big-endian encoding; round-tripped symmetrically in Verify.

	mac := computeMAC(key, buf[:9])
	copy(buf[9:], mac)

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

	expected := computeMAC(key, payload[:9])
	if !hmac.Equal(expected, payload[9:]) || malformed {
		if malformed {
			return ErrMalformed
		}
		return ErrBadMAC
	}

	if payload[0] != Version {
		return ErrUnsupportedVersion
	}

	expMs := int64(binary.BigEndian.Uint64(payload[1:9])) // #nosec G115 -- symmetric reinterpretation of the big-endian bit pattern encoded by Mint.
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
