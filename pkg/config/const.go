package config

import (
	"log/slog"
	"time"
)

const LOG_LEVEL_DISABLED = slog.LevelDebug - 1

const REQUEST_TIMEOUT = 10 * time.Second
const SHUTDOWN_TIMEOUT = 5 * time.Second

// Server-level timeouts to mitigate Slowloris and connection exhaustion attacks.
const READ_HEADER_TIMEOUT = 5 * time.Second
const READ_TIMEOUT = 30 * time.Second
const WRITE_TIMEOUT = 30 * time.Second
const IDLE_TIMEOUT = 120 * time.Second

// MAX_HEADER_BYTES limits the size of request headers (64 KB).
const MAX_HEADER_BYTES = 1 << 16

const M3U8_MIME_TYPE = "application/vnd.apple.mpegurl"
const MP4_MIME_TYPE = "video/mp4"

// DefaultMediaWindowSize is the maximum number of segments in the media playlist
// sliding window.
const DefaultMediaWindowSize = 12

// DefaultStreamPort is the default port for the public HLS stream server.
const DefaultStreamPort = 8080

// DefaultAPIPort is the default port for the authenticated push API server.
const DefaultAPIPort = 8081

// MinTokenLength is the minimum number of characters required for a stream token.
const MinTokenLength = 32
