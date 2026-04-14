// Package viewer provides stateless viewer tokens for HLS stream playback.
//
// A token is a compact, URL-safe binary blob consisting of:
//
//	[1 byte version][1 byte type][4 bytes uint32 big-endian unix minutes][16 bytes truncated HMAC-SHA256]
//
// The MAC covers the version byte, the type byte, and the expiration bytes,
// keyed by a per-stream secret. Tokens are encoded with base64.RawURLEncoding,
// yielding a fixed 30-character string. Tokens are not bound to a stream ID;
// isolation across streams is provided by using a distinct signing key per
// stream.
//
// The type byte distinguishes capability classes of token so the HTTP layer
// can refuse tokens minted for one purpose (e.g. segment/init fetches) on a
// route serving another (e.g. the media playlist). Without this, a holder of
// a playlist-scoped short-lived token could refetch the media playlist to
// rotate their token indefinitely, defeating the TTL.
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

// Type identifies the capability class of a token. The MAC covers the type
// byte so a token cannot be re-typed without the signing key.
type Type byte

const (
	// TypeViewer is the token class minted via the push-authenticated
	// POST /viewer_token endpoint. It represents direct operator intent
	// to grant a named viewer access and is accepted on ALL stream
	// routes (including playlists).
	TypeViewer Type = 1

	// TypeSegment is the short-lived token class minted internally by
	// the media-playlist renderer and baked into init/segment URIs. It
	// is accepted ONLY on init/segment routes — never on playlists —
	// so that a holder cannot refetch the playlist to rotate into a
	// freshly-minted token and defeat the TTL.
	TypeSegment Type = 2
)

// macLen is the truncated HMAC length in bytes (128-bit).
const macLen = 16

// headerLen is the number of MAC-covered leading bytes:
// version (1) + type (1) + uint32 minutes (4).
const headerLen = 1 + 1 + 4

// TokenBytes is the length of a decoded token in bytes: header + MAC = 22.
const TokenBytes = headerLen + macLen

// EncodedTokenLen is the length of a base64url-encoded (no padding) token.
const EncodedTokenLen = 30

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

// Mint produces a token of the given type for the given expiration (unix ms).
// The expiry is internally floored to the preceding minute boundary before
// encoding; see the package comment for rationale. The key must be non-empty;
// callers are expected to enforce a minimum length upstream.
//
// Returns ErrMalformed if the minute-aligned expiration is negative or does
// not fit in a uint32 (overflow after year ~10140).
func Mint(key []byte, expiresAtMs int64, typ Type) (string, error) {
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
	buf[1] = byte(typ)
	binary.BigEndian.PutUint32(buf[2:headerLen], uint32(expMinutes))

	mac := computeMAC(key, buf[:headerLen])
	copy(buf[headerLen:], mac)

	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// Verify decodes token and checks its MAC and expiration against now. On
// success it returns the token's Type; callers are responsible for rejecting
// types that are not allowed on the current route. On failure it returns a
// zero Type and one of the sentinel errors above.
//
// Timing is kept uniform across MAC failures by always running the HMAC
// compare; malformed inputs are compared against a zeroed canonical payload
// so the MAC path still executes.
func Verify(key []byte, now time.Time, token string) (Type, error) {
	if len(key) == 0 {
		return 0, ErrEmptyKey
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

	expected := computeMAC(key, payload[:headerLen])
	if !hmac.Equal(expected, payload[headerLen:]) || malformed {
		if malformed {
			return 0, ErrMalformed
		}
		return 0, ErrBadMAC
	}

	if payload[0] != Version {
		return 0, ErrUnsupportedVersion
	}

	expMinutes := binary.BigEndian.Uint32(payload[2:headerLen])
	expMs := int64(expMinutes) * msPerMinute
	if expMs <= now.UnixMilli() {
		return 0, ErrExpired
	}

	return Type(payload[1]), nil
}

// computeMAC returns the truncated HMAC-SHA256 of msg under key.
func computeMAC(key, msg []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	sum := h.Sum(nil)
	return sum[:macLen]
}
