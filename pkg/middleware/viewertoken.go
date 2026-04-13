package middleware

import (
	"log/slog"
	"net/http"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/dskiff/streamloom/pkg/config"
	"github.com/dskiff/streamloom/pkg/viewer"
	"github.com/go-chi/chi/v5"
)

// ViewerTokenAuth returns a middleware that enforces stateless viewer tokens
// on stream routes. It must be mounted inside a {streamID} route group so
// chi.URLParam("streamID") resolves.
//
// Behavior:
//   - Invalid stream ID format: 404.
//   - Stream has no configured viewer-token key: pass through (feature is
//     opt-in per stream; preserves backward-compatible public playback).
//   - Stream has a key and the request lacks a valid `vt` query param: 401.
//     Validation failures (missing, malformed, bad MAC, expired, unsupported
//     version) are all logged at warn but collapsed to a single 401 so
//     attackers can't distinguish failure modes.
//
// This middleware must be placed BEFORE any watcher-recording middleware so
// that unauthorized requests do not inflate the active-viewer count.
func ViewerTokenAuth(clk clock.Clock, keys map[string][]byte, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			streamID := chi.URLParam(r, "streamID")
			if err := config.ValidateStreamID(streamID); err != nil {
				logger.Warn("invalid stream ID", "value", streamID, "error", err)
				w.WriteHeader(http.StatusNotFound)
				return
			}

			key, ok := keys[streamID]
			if !ok {
				// Viewer-token auth is opt-in per stream. Unconfigured
				// streams remain publicly accessible.
				next.ServeHTTP(w, r)
				return
			}

			vt := r.URL.Query().Get("vt")
			if err := viewer.Verify(key, clk.Now(), vt); err != nil {
				logger.Warn("viewer token rejected",
					"streamID", streamID,
					"method", r.Method,
					"path", r.URL.Path,
					"reason", err.Error(),
				)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
