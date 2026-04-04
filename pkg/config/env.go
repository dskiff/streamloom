package config

import (
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MaxStreamIDLength is the upper bound on stream ID length.
const MaxStreamIDLength = 512

// streamIDRegexp matches valid stream IDs: non-empty, alphanumeric only.
var streamIDRegexp = regexp.MustCompile(`^[a-zA-Z0-9]+$`)

// ValidateStreamID checks that s is a valid stream ID: non-empty, at most
// MaxStreamIDLength characters, and alphanumeric only.
func ValidateStreamID(s string) error {
	if s == "" {
		return fmt.Errorf("stream ID must not be empty")
	}
	if len(s) > MaxStreamIDLength {
		return fmt.Errorf("stream ID must be at most %d characters, got %d", MaxStreamIDLength, len(s))
	}
	if !streamIDRegexp.MatchString(s) {
		return fmt.Errorf("stream ID %q contains invalid characters (must be alphanumeric)", s)
	}
	return nil
}

// TokenDigest is a fixed-size SHA-256 digest of a "Bearer <token>" header value.
type TokenDigest = [sha256.Size]byte

// DefaultStreamMaxBufferBytes is the default maximum total buffer size per
// stream (segmentCapacity * segmentBytes). 1 GiB.
const DefaultStreamMaxBufferBytes int64 = 1 << 30

// DefaultBufferWorkingSpace is the number of extra BufferPool slots beyond the
// ring buffer capacity. These slots allow HTTP handlers to acquire a buffer
// before committing to the ring buffer.
const DefaultBufferWorkingSpace = 20

type Env struct {
	DEBUG bool

	// REQUEST_LOG_FILE is the file path for HTTP request/response logs.
	// When non-empty, request logs are written to this file in append mode.
	// When empty, request logging is disabled.
	REQUEST_LOG_FILE string

	// STREAM_PORT is the port for the public HLS stream server.
	STREAM_PORT int

	// API_PORT is the port for the authenticated push API server.
	API_PORT int

	// STREAM_MAX_BUFFER_BYTES is the maximum allowed total buffer size
	// (segmentCapacity * segmentBytes) for a single stream init request.
	STREAM_MAX_BUFFER_BYTES int64

	// BUFFER_WORKING_SPACE is the number of extra BufferPool slots beyond the
	// ring buffer capacity, allowing concurrent HTTP handlers to hold buffers
	// before committing them to the ring buffer.
	BUFFER_WORKING_SPACE int

	// BIND_ADDR is the IP address to bind both servers to.
	// When empty, the default is determined by the caller (127.0.0.1 in dev,
	// 0.0.0.0 in production). Override via SL_BIND_ADDR for non-container deployments.
	BIND_ADDR string

	// TRUSTED_PROXIES is a list of CIDR ranges whose requests are trusted to
	// provide accurate X-Forwarded-For / X-Real-IP headers. When empty, forwarded
	// headers are never trusted (safe default).
	TRUSTED_PROXIES []*net.IPNet

	// STREAM_IDLE_TIMEOUT is the duration after which a stream with no new
	// content is automatically deleted. Set to 0 to disable reaping.
	STREAM_IDLE_TIMEOUT time.Duration

	// STREAM_TOKENS maps stream IDs to SHA-256 digests of the expected
	// "Bearer <token>" header value for constant-time comparison.
	STREAM_TOKENS map[string]TokenDigest
}

// GetStreamToken returns the SHA-256 digest of the expected "Bearer <token>" header
// for a stream ID. Returns the zero digest and false if the stream has no configured token.
func (e *Env) GetStreamToken(streamID string) (TokenDigest, bool) {
	tok, ok := e.STREAM_TOKENS[streamID]
	return tok, ok
}

// GetEnv reads configuration from environment variables.
// Stream tokens (SL_STREAM_<id>_TOKEN) are unset from the environment after reading.
func GetEnv() (Env, error) {
	idleTimeout, err := parseStreamIdleTimeout()
	if err != nil {
		return Env{}, err
	}

	streamTokens, err := parseStreamTokens()
	if err != nil {
		return Env{}, err
	}

	maxBufferBytes, err := parseStreamMaxBufferBytes()
	if err != nil {
		return Env{}, err
	}

	workingSpace, err := parseBufferWorkingSpace()
	if err != nil {
		return Env{}, err
	}

	trustedProxies, err := parseTrustedProxies()
	if err != nil {
		return Env{}, err
	}

	bindAddr, err := parseBindAddr()
	if err != nil {
		return Env{}, err
	}

	streamPort, err := parsePort("SL_STREAM_PORT", DefaultStreamPort)
	if err != nil {
		return Env{}, err
	}

	apiPort, err := parsePort("SL_API_PORT", DefaultAPIPort)
	if err != nil {
		return Env{}, err
	}

	return Env{
		DEBUG:                   trueish(os.Getenv("SL_DEBUG")),
		REQUEST_LOG_FILE:        os.Getenv("SL_REQUEST_LOG_FILE"),
		BIND_ADDR:               bindAddr,
		STREAM_PORT:             streamPort,
		API_PORT:                apiPort,
		STREAM_MAX_BUFFER_BYTES: maxBufferBytes,
		BUFFER_WORKING_SPACE:    workingSpace,
		TRUSTED_PROXIES:         trustedProxies,
		STREAM_IDLE_TIMEOUT:     idleTimeout,

		STREAM_TOKENS: streamTokens,
	}, nil
}

// parseStreamMaxBufferBytes reads SL_STREAM_MAX_BUFFER_BYTES from the
// environment. Returns DefaultStreamMaxBufferBytes when the variable is unset
// or empty. Returns an error for non-numeric or non-positive values.
func parseStreamMaxBufferBytes() (int64, error) {
	raw := os.Getenv("SL_STREAM_MAX_BUFFER_BYTES")
	if raw == "" {
		return DefaultStreamMaxBufferBytes, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid SL_STREAM_MAX_BUFFER_BYTES value %q: %w", raw, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf("SL_STREAM_MAX_BUFFER_BYTES must be positive, got %d", v)
	}
	return v, nil
}

// parseBufferWorkingSpace reads SL_BUFFER_WORKING_SPACE from the environment.
// Returns DefaultBufferWorkingSpace when the variable is unset or empty.
// Returns an error for non-numeric or negative values.
func parseBufferWorkingSpace() (int, error) {
	raw := os.Getenv("SL_BUFFER_WORKING_SPACE")
	if raw == "" {
		return DefaultBufferWorkingSpace, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid SL_BUFFER_WORKING_SPACE value %q: %w", raw, err)
	}
	if v < 0 {
		return 0, fmt.Errorf("SL_BUFFER_WORKING_SPACE must be non-negative, got %d", v)
	}
	return v, nil
}

// parseTrustedProxies reads SL_TRUSTED_PROXIES from the environment.
// The value is a comma-separated list of CIDR ranges (e.g. "10.0.0.0/8,172.16.0.0/12").
// Returns nil (no trusted proxies) when the variable is unset or empty.
func parseTrustedProxies() ([]*net.IPNet, error) {
	raw := os.Getenv("SL_TRUSTED_PROXIES")
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	nets := make([]*net.IPNet, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		_, cidr, err := net.ParseCIDR(p)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q in SL_TRUSTED_PROXIES: %w", p, err)
		}
		nets = append(nets, cidr)
	}
	return nets, nil
}

// parseBindAddr reads SL_BIND_ADDR from the environment.
// Returns the value if set and valid, empty string if unset.
// Returns an error if the value is not a valid IP address.
func parseBindAddr() (string, error) {
	raw := os.Getenv("SL_BIND_ADDR")
	if raw == "" {
		return "", nil
	}
	if net.ParseIP(raw) == nil {
		return "", fmt.Errorf("invalid SL_BIND_ADDR value %q: not a valid IP address", raw)
	}
	return raw, nil
}

// parseStreamIdleTimeout reads SL_STREAM_IDLE_TIMEOUT from the environment.
// Returns DefaultStreamIdleTimeout when the variable is unset or empty.
// Accepts Go duration strings (e.g. "2m", "90s"). A value of "0" disables
// reaping. Returns an error for negative or unparseable values.
func parseStreamIdleTimeout() (time.Duration, error) {
	raw := os.Getenv("SL_STREAM_IDLE_TIMEOUT")
	if raw == "" {
		return DefaultStreamIdleTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid SL_STREAM_IDLE_TIMEOUT value %q: %w", raw, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("SL_STREAM_IDLE_TIMEOUT must be non-negative, got %s", d)
	}
	return d, nil
}

const streamTokenPrefix = "SL_STREAM_"
const streamTokenSuffix = "_TOKEN"

// parseStreamTokens scans the environment for SL_STREAM_<id>_TOKEN variables,
// validates that <id> is a valid stream ID and the token is non-empty, stores a
// SHA-256 digest of the expected bearer header, and unsets the env var.
func parseStreamTokens() (map[string]TokenDigest, error) {
	tokens := make(map[string]TokenDigest)

	for _, kv := range os.Environ() {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}

		if !strings.HasPrefix(key, streamTokenPrefix) || !strings.HasSuffix(key, streamTokenSuffix) {
			continue
		}

		// Guard against prefix/suffix overlap (e.g. "SL_STREAM_TOKEN" where
		// the prefix and suffix share characters, producing an invalid slice).
		if len(key) < len(streamTokenPrefix)+len(streamTokenSuffix) {
			continue
		}

		// Extract the stream ID between prefix and suffix.
		streamIDStr := key[len(streamTokenPrefix) : len(key)-len(streamTokenSuffix)]
		if streamIDStr == "" {
			return nil, fmt.Errorf("empty stream ID in env var %s", key)
		}

		if err := ValidateStreamID(streamIDStr); err != nil {
			return nil, fmt.Errorf("invalid stream ID %q in env var %s: %w", streamIDStr, key, err)
		}

		if value == "" {
			return nil, fmt.Errorf("empty token value for env var %s", key)
		}
		if len(value) < MinTokenLength {
			return nil, fmt.Errorf("token for env var %s is too short (%d chars); minimum is %d", key, len(value), MinTokenLength)
		}
		tokens[streamIDStr] = sha256.Sum256([]byte("Bearer " + value))

		// Clear the env var after reading.
		os.Unsetenv(key)
	}

	return tokens, nil
}

// parsePort reads the named environment variable and returns the port number.
// Returns defaultPort when the variable is unset or empty.
func parsePort(envVar string, defaultPort int) (int, error) {
	raw := os.Getenv(envVar)
	if raw == "" {
		return defaultPort, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s value %q: %w", envVar, raw, err)
	}
	if v <= 0 || v > 65535 {
		return 0, fmt.Errorf("%s must be between 1 and 65535, got %d", envVar, v)
	}
	return v, nil
}

func trueish(s string) bool {
	if s == "" {
		return false
	}
	if s == "0" {
		return false
	}
	if strings.ToLower(s) == "false" {
		return false
	}
	return true
}
