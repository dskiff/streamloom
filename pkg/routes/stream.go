package routes

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/dskiff/streamloom/pkg/config"
	mw "github.com/dskiff/streamloom/pkg/middleware"
	"github.com/dskiff/streamloom/pkg/pool"
	"github.com/dskiff/streamloom/pkg/stream"
	"github.com/dskiff/streamloom/pkg/watcher"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// writeStreamUnavailable sends a 503 with Retry-After for configured-but-uninitialized streams.
func writeStreamUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "2")
	w.WriteHeader(http.StatusServiceUnavailable)
}

// getStream validates the stream ID and looks up the stream. Returns the stream
// and 0 on success, or nil and an HTTP status code:
// 404 if the stream ID is invalid, or 503 if the stream is not yet active.
// All valid-format IDs that lack an active stream return 503 uniformly to
// prevent enumerating configured stream IDs via response-code differentiation.
func getStream(store *stream.Store, streamID string) (*stream.Stream, int) {
	if err := config.ValidateStreamID(streamID); err != nil {
		return nil, http.StatusNotFound
	}
	s := store.Get(streamID)
	if s != nil {
		return s, 0
	}
	return nil, http.StatusServiceUnavailable
}

// Stream constructs the chi router for the public HLS stream server.
func Stream(logger *slog.Logger, env config.Env, store *stream.Store, requestLogger *slog.Logger, tracker *watcher.Tracker) chi.Router {
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(mw.TrustedRealIP(env.TRUSTED_PROXIES))
	router.Use(requestLogMiddleware(logger, requestLogger))
	router.Use(middleware.Recoverer)
	router.Use(mw.UnrecoverableGuard)
	router.Use(middleware.SetHeader("X-Content-Type-Options", "nosniff"))
	router.Use(middleware.SetHeader("X-Frame-Options", "DENY"))
	router.Use(middleware.SetHeader("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'"))
	router.Use(middleware.Timeout(config.REQUEST_TIMEOUT))

	router.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	router.Route("/stream/{streamID}", func(r chi.Router) {
		// ViewerTokenAuth runs BEFORE RecordWatcher so that 401 responses
		// do not inflate the active-viewer count.
		r.Use(mw.ViewerTokenAuth(store.Clock(), env.STREAM_VIEWER_TOKEN_KEYS, logger))
		r.Use(mw.RecordWatcher(tracker))

		r.Get("/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
			streamID := chi.URLParam(r, "streamID")
			logger.Debug("handling media request", "streamID", streamID)

			s, status := getStream(store, streamID)
			if s == nil {
				if status == http.StatusServiceUnavailable {
					writeStreamUnavailable(w)
				} else {
					w.WriteHeader(status)
				}
				return
			}

			// Block until a valid playlist is available, the stream is
			// deleted, or the request is cancelled.
			select {
			case <-s.HasPlaylist():
			case <-s.Done():
				writeStreamUnavailable(w)
				return
			case <-r.Context().Done():
				writeStreamUnavailable(w)
				return
			}

			playlist := s.CachedPlaylist()
			if playlist == "" {
				// All segments were evicted between the HasPlaylist gate
				// and now.  Tell the player to retry rather than serving
				// an empty body.
				writeStreamUnavailable(w)
				return
			}
			// Substitute the per-viewer query placeholder. When the request
			// did not carry a vt (public stream), placeholders are stripped.
			playlist = stream.ResolveViewerToken(playlist, r.URL.Query().Get("vt"))
			w.Header().Set("Content-Type", config.M3U8_MIME_TYPE)
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Length", strconv.Itoa(len(playlist)))
			if _, err := io.WriteString(w, playlist); err != nil { // #nosec G705 -- playlist contains only numeric data from internal state, not user input
				logger.Error("failed to write response", "error", err)
			}
		})

		r.Get("/stream.m3u8", func(w http.ResponseWriter, r *http.Request) {
			streamID := chi.URLParam(r, "streamID")
			logger.Debug("handling stream request", "streamID", streamID)

			s, status := getStream(store, streamID)
			if s == nil {
				if status == http.StatusServiceUnavailable {
					writeStreamUnavailable(w)
				} else {
					w.WriteHeader(status)
				}
				return
			}

			meta := s.Metadata()

			// Propagate ?vt= from the incoming request into the media
			// playlist URI. HLS players do not carry a parent query string
			// over to relative URIs, so each emitted URI needs its own copy.
			mediaURI := "media.m3u8"
			if vt := r.URL.Query().Get("vt"); vt != "" {
				mediaURI += "?vt=" + url.QueryEscape(vt)
			}

			builder := strings.Builder{}
			builder.WriteString("#EXTM3U\n")
			builder.WriteString("#EXT-X-VERSION:7\n")
			builder.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
			builder.WriteString(
				fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d,CODECS=\"%s\",FRAME-RATE=%.3f\n", meta.Bandwidth, meta.Width, meta.Height, meta.Codecs, meta.FrameRate),
			)
			builder.WriteString(mediaURI)
			builder.WriteByte('\n')

			w.Header().Set("Content-Type", config.M3U8_MIME_TYPE)
			w.Header().Set("Cache-Control", "no-cache")
			body := builder.String()
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			if _, err := io.WriteString(w, body); err != nil {
				logger.Error("failed to write response", "error", err)
			}
		})

		r.Get("/init.mp4", func(w http.ResponseWriter, r *http.Request) {
			streamID := chi.URLParam(r, "streamID")
			logger.Debug("handling init segment request", "streamID", streamID)

			s, status := getStream(store, streamID)
			if s == nil {
				if status == http.StatusServiceUnavailable {
					writeStreamUnavailable(w)
				} else {
					w.WriteHeader(status)
				}
				return
			}

			initData, ok := s.GetInit()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}

			w.Header().Set("Content-Type", config.MP4_MIME_TYPE)
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Length", strconv.Itoa(len(initData)))
			if _, err := w.Write(initData); err != nil {
				logger.Error("failed to write response", "error", err)
			}
		})

		r.Get("/segment_{segmentID}.m4s", func(w http.ResponseWriter, r *http.Request) {
			streamID := chi.URLParam(r, "streamID")
			segmentIDStr := chi.URLParam(r, "segmentID")
			logger.Debug("handling segment request", "streamID", streamID, "segmentID", segmentIDStr)

			s, status := getStream(store, streamID)
			if s == nil {
				if status == http.StatusServiceUnavailable {
					writeStreamUnavailable(w)
				} else {
					w.WriteHeader(status)
				}
				return
			}

			segmentID, err := strconv.ParseUint(segmentIDStr, 10, 32)
			if err != nil {
				logger.Warn("invalid segmentID", "value", segmentIDStr, "error", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			err = s.RunWithSegmentSlot(uint32(segmentID), func(slot *pool.BufferSlot) error {
				w.Header().Set("Content-Type", config.MP4_MIME_TYPE)
				w.Header().Set("Content-Length", strconv.Itoa(slot.Len()))
				w.Header().Set("Cache-Control", "no-cache")
				_, err := slot.WriteTo(w)
				return err
			})
			if err != nil {
				if errors.Is(err, stream.ErrSegmentNotFound) {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				logger.Error("failed to write response", "error", err)
			}
		})
	})

	return router
}
