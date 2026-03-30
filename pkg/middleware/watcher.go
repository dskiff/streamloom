package middleware

import (
	"net"
	"net/http"

	"github.com/dskiff/streamloom/pkg/watcher"
	"github.com/go-chi/chi/v5"
)

// RecordWatcher returns a middleware that records the client IP for watcher
// tracking on every request within a {streamID} route group. It extracts
// the stream ID from the chi URL parameter and the client IP from RemoteAddr
// (which has already been resolved by TrustedRealIP).
func RecordWatcher(tracker *watcher.Tracker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			streamID := chi.URLParam(r, "streamID")
			if streamID != "" {
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
