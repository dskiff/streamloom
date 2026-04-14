// Package viewer provides stateless viewer tokens for HLS stream playback.
//
// A token is a compact, URL-safe binary blob consisting of:
//
//	[1 byte version][4 bytes uint32 big-endian unix minutes][16 bytes truncated HMAC-SHA256]
//
// The MAC covers the version byte and the expiration bytes, keyed by a
// per-(stream, type) derived key (see DeriveKey). Tokens are encoded with
// base64.RawURLEncoding in Strict mode, yielding a fixed 28-character string
// with a single canonical encoding per 21-byte payload.
//
// Neither the stream ID nor the token's capability class (Type) appears in
// the token payload. Both are bound into the signing key via the KDF, so a
// token minted for one (stream, type) pair cannot verify under any other —
// this preserves tamper-resistance of the scoping without spending payload
// bytes. The HTTP layer enforces per-route capability by running Verify with
// the candidate type's derived key(s); success identifies the type
// implicitly.
//
// The expiration is encoded with 1-minute precision: the ms-granularity
// value passed to Mint is truncated (toward zero) to the minute boundary.
// Callers that need to know the exact encoded expiry should truncate their
// input to a minute beforehand (Mint does this internally regardless). The
// range of a uint32 minute counter reaches beyond year 10000, so practical
// overflow is impossible.
//
// Forward-compatibility note: because the MAC covers the version byte,
// introducing a new wire-format version requires either a parallel-verify
// period (accept both old and new) or a key rotation at the cut-over. A
// token with an unknown version but a valid MAC is rejected by Verify
// before any version-specific parsing runs, so downgrade-style attacks are
// not possible. The wire-format Version byte is independent of the KDF
// domain tag (kdfDomainTag); they evolve on separate axes.
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

// Type identifies the capability class of a token. It is NOT serialized
// into the token payload; instead it selects the signing key via DeriveKey,
// so a token minted with one type's derived key cannot verify under any
// other type's derived key. Distinct byte values are retained because
// callers use Type as a map key and for logging.
type Type byte

const (
	// TypePlaylist is the token class minted via the push-authenticated
	// POST /viewer_token endpoint. It represents direct operator intent
	// to grant a named viewer access. Despite the name (chosen because
	// playlist routes are the exclusive class that accept it), a
	// TypePlaylist token is also accepted on init/segment routes so that
	// operator-granted tokens work for the full playback flow.
	TypePlaylist Type = 1

	// TypeSegment is the short-lived token class minted internally by
	// the media-playlist renderer and baked into init/segment URIs. It
	// is accepted ONLY on init/segment routes — never on playlists —
	// so that a holder cannot refetch the playlist to rotate into a
	// freshly-minted token and defeat the TTL.
	TypeSegment Type = 2
)

// kdfDomainTag is prepended to every KDF input so tokens minted under this
// scheme cannot be confused with HMACs produced by any other use of the
// same env secret. Versioned so the KDF can evolve independently of the
// wire-format Version byte.
const kdfDomainTag = "streamloom/viewer-token/v1"

// typeString returns the canonical KDF input string for a Type. The
// vocabulary is fixed so it cannot collide with any alphanumeric streamID
// under the zero-byte separator used by DeriveKey.
func typeString(t Type) (string, bool) {
	switch t {
	case TypePlaylist:
		return "playlist", true
	case TypeSegment:
		return "segment", true
	default:
		return "", false
	}
}

// macLen is the truncated HMAC length in bytes (128-bit).
const macLen = 16

// headerLen is the number of MAC-covered leading bytes:
// version (1) + uint32 minutes (4).
const headerLen = 1 + 4

// TokenBytes is the length of a decoded token in bytes: header + MAC = 21.
const TokenBytes = headerLen + macLen

// EncodedTokenLen is the length of a base64url-encoded (no padding) token.
const EncodedTokenLen = 28

// msPerMinute is the resolution of the encoded expiration field. Kept
// package-private because the minute unit does not leak beyond serde.
const msPerMinute = 60_000

// tokenEncoding is the base64 encoding used for tokens: URL-safe alphabet,
// no padding, and strict decoding. Strict mode rejects non-canonical
// encodings whose unused trailing bits are non-zero. Without strict mode,
// a single 21-byte payload would have multiple valid base64url encodings —
// a canonicalization gap with no practical security impact but a source of
// confusion for logging, caching, or deduplication layers that treat the
// string form as the identifier.
var tokenEncoding = base64.RawURLEncoding.Strict()

// Sentinel errors returned by Verify / Mint / DeriveKey. HTTP handlers
// should collapse all Verify errors to a single 401 response; the
// distinction exists only for logging.
var (
	ErrMalformed          = errors.New("viewer: malformed token")
	ErrUnsupportedVersion = errors.New("viewer: unsupported token version")
	ErrBadMAC             = errors.New("viewer: bad MAC")
	ErrExpired            = errors.New("viewer: token expired")
	ErrEmptyKey           = errors.New("viewer: signing key must not be empty")
	ErrUnknownType        = errors.New("viewer: unknown token type")
)

// DeriveKey returns the per-(stream, type) HMAC-SHA256 signing key derived
// from the raw env-var secret. Binding the stream ID and type into the key
// means a token minted for one (stream, type) pair cannot be replayed
// against any other pair — the MAC check fails before the token payload is
// ever inspected for scoping.
//
// The KDF is HMAC-SHA256 as a PRF over a domain-separated context:
//
//	HMAC-SHA256(envKey, kdfDomainTag || 0x00 || streamID || 0x00 || typeString(typ))
//
// The 0x00 separators prevent ambiguity at the streamID/typeString
// boundary. streamID is alphanumeric (enforced by config.ValidateStreamID)
// and typeString is drawn from a fixed internal vocabulary, so neither can
// contain 0x00.
//
// Returns ErrEmptyKey if envKey is empty and ErrUnknownType if typ is not
// a recognized Type. Callers are expected to enforce a minimum env-key
// length upstream (see config.MinTokenLength).
func DeriveKey(envKey []byte, streamID string, typ Type) ([]byte, error) {
	if len(envKey) == 0 {
		return nil, ErrEmptyKey
	}
	ts, ok := typeString(typ)
	if !ok {
		return nil, ErrUnknownType
	}
	h := hmac.New(sha256.New, envKey)
	h.Write([]byte(kdfDomainTag))
	h.Write([]byte{0})
	h.Write([]byte(streamID))
	h.Write([]byte{0})
	h.Write([]byte(ts))
	return h.Sum(nil), nil
}

// Mint produces a token for the given expiration (unix ms) under the given
// derived signing key. The expiry is internally truncated (toward zero) to
// the minute boundary before encoding; see the package comment for
// rationale. The key must be non-empty; callers are expected to pass a key
// returned by DeriveKey.
//
// Returns ErrMalformed if expiresAtMs is negative or its minute value does
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
	binary.BigEndian.PutUint32(buf[1:headerLen], uint32(expMinutes))

	mac := computeMAC(key, buf[:headerLen])
	copy(buf[headerLen:], mac)

	return tokenEncoding.EncodeToString(buf[:]), nil
}

// Verify decodes token and checks its MAC and expiration against now using
// the supplied derived signing key. On failure it returns one of the
// sentinel errors above; callers should collapse all failures to a single
// 401. A successful Verify implies the token was minted under the same
// derived key — i.e. for the same (stream, type) pair.
//
// Timing is kept uniform across MAC failures by always running the HMAC
// compare; malformed inputs are compared against a zeroed canonical payload
// so the MAC path still executes.
func Verify(key []byte, now time.Time, token string) error {
	if len(key) == 0 {
		return ErrEmptyKey
	}

	var payload [TokenBytes]byte
	malformed := false

	// Strict decoding: rejects non-canonical encodings (e.g. unused trailing
	// bits set) so a given payload has exactly one valid string form. Prevents
	// silent multiple representations of the same token.
	raw, err := tokenEncoding.DecodeString(token)
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
			return ErrMalformed
		}
		return ErrBadMAC
	}

	if payload[0] != Version {
		return ErrUnsupportedVersion
	}

	expMinutes := binary.BigEndian.Uint32(payload[1:headerLen])
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
