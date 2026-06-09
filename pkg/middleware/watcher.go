package middleware

import (
	"net"
	"net/http"

	"github.com/dskiff/streamloom/pkg/watcher"
	"github.com/go-chi/chi/v5"
)

// RecordWatcher returns a middleware that records the client IP for watcher
// tracking on requests to streams that currently exist. It extracts the stream
// ID from the chi URL parameter and the client IP from RemoteAddr (which has
// already been resolved by TrustedRealIP).
//
// streamExists gates recording so that only streams the store actually holds
// are tracked. The stream ID is taken straight from the URL path, so without
// this gate a client could request arbitrary IDs and mint an unbounded number
// of tracked streams in the tracker's in-memory map. Gating on existence both
// bounds that map (to the configured/initialized streams) and keeps the
// active-watcher metric free of phantom streams.
func RecordWatcher(tracker *watcher.Tracker, streamExists func(streamID string) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			streamID := chi.URLParam(r, "streamID")
			if streamID != "" && streamExists(streamID) {
				ip := extractIP(r.RemoteAddr)
				tracker.Record(streamID, ip)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// extractIP parses the host portion from an address that may be "IP",
// "IP:port", or "[IPv6]:port". After chi's RealIP middleware, RemoteAddr
// may be a bare IP (from X-Forwarded-For) or IP:port (direct connection).
func extractIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// addr is likely a bare IP without port
		return addr
	}
	return host
}
