package routes

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dskiff/streamloom/pkg/config"
	mw "github.com/dskiff/streamloom/pkg/middleware"
	"github.com/dskiff/streamloom/pkg/pool"
	"github.com/dskiff/streamloom/pkg/stream"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// contextKey is a private type for context keys to avoid collisions.
type contextKey string

// streamIDKey is the context key for the validated string stream ID.
const streamIDKey contextKey = "streamID"

// maxResolutionDimension is the upper bound for width/height (covers up to 32K).
const maxResolutionDimension = 32768

// parseResolution parses a resolution string like "1920x1080" into width and height.
func parseResolution(s string) (width, height int, ok bool) {
	parts := strings.SplitN(s, "x", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, err := strconv.Atoi(parts[0])
	if err != nil || w <= 0 || w > maxResolutionDimension {
		return 0, 0, false
	}
	h, err := strconv.Atoi(parts[1])
	if err != nil || h <= 0 || h > maxResolutionDimension {
		return 0, 0, false
	}
	return w, h, true
}

// API constructs the chi router for the authenticated push API server.
func API(logger *slog.Logger, env config.Env, store *stream.Store, requestLogger *slog.Logger) chi.Router {
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

	// dummyDigest is a SHA-256 digest used for constant-time comparison when a
	// stream has no configured token. We always compare fixed-size digests so
	// that ConstantTimeCompare never short-circuits on length mismatch.
	dummyDigest := sha256.Sum256([]byte("Bearer __dummy_comparison_target__"))

	router.Route("/api/v1/stream/{streamID}", func(r chi.Router) {
		// Per-stream token auth middleware. The middleware is inside the
		// {streamID} route group so chi.URLParam is available.
		r.Use(func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				streamID := chi.URLParam(r, "streamID")
				if err := config.ValidateStreamID(streamID); err != nil {
					logger.Warn("invalid stream ID", "value", streamID, "error", err)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				tokenDigest := sha256.Sum256([]byte(r.Header.Get("Authorization")))

				// Always compare against the configured token or a dummy
				// digest so that ConstantTimeCompare is always executed and
				// participates in the auth decision — preventing timing leaks
				// and dead-code elimination. Unconfigured streams are still
				// rejected via the !ok condition.
				expected, ok := env.GetStreamToken(streamID)
				if !ok {
					expected = dummyDigest
				}
				if subtle.ConstantTimeCompare(tokenDigest[:], expected[:]) != 1 || !ok {
					if !ok {
						logger.Warn("unauthorized request: stream not configured", "method", r.Method, "path", r.URL.Path, "streamID", streamID)
					} else {
						logger.Warn("unauthorized request: invalid token", "method", r.Method, "path", r.URL.Path, "streamID", streamID)
					}
					w.WriteHeader(http.StatusUnauthorized)
					return
				}

				ctx := context.WithValue(r.Context(), streamIDKey, streamID)
				h.ServeHTTP(w, r.WithContext(ctx))
			})
		})
		r.Delete("/", func(w http.ResponseWriter, r *http.Request) {
			streamID := r.Context().Value(streamIDKey).(string)
			logger.Debug("handling stream delete request", "streamID", streamID)

			if !store.Delete(streamID) {
				w.WriteHeader(http.StatusNotFound)
				return
			}

			logger.Info("stream deleted", "streamID", streamID)
			w.WriteHeader(http.StatusNoContent)
		})

		r.Post("/init", func(w http.ResponseWriter, r *http.Request) {
			streamID := r.Context().Value(streamIDKey).(string)
			logger.Debug("handling stream init request", "streamID", streamID)

			// Parse required metadata headers.
			bandwidthStr := r.Header.Get("X-SL-BANDWIDTH")
			if bandwidthStr == "" {
				logger.Warn("missing bandwidth header")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			bandwidth, err := strconv.Atoi(bandwidthStr)
			if err != nil || bandwidth <= 0 {
				logger.Warn("invalid bandwidth header", "value", bandwidthStr, "error", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			codecs := r.Header.Get("X-SL-CODECS")
			if err := stream.ValidateCodecs(codecs); err != nil {
				logger.Warn("invalid codecs header", "value", codecs, "error", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			resolutionStr := r.Header.Get("X-SL-RESOLUTION")
			if resolutionStr == "" {
				logger.Warn("missing resolution header")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			width, height, ok := parseResolution(resolutionStr)
			if !ok {
				logger.Warn("invalid resolution header", "value", resolutionStr)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			framerateStr := r.Header.Get("X-SL-FRAMERATE")
			if framerateStr == "" {
				logger.Warn("missing framerate header")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			framerate, err := strconv.ParseFloat(framerateStr, 64)
			if err != nil || math.IsNaN(framerate) || math.IsInf(framerate, 0) || framerate <= 0 {
				logger.Warn("invalid framerate header", "value", framerateStr, "error", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			targetDurationStr := r.Header.Get("X-SL-TARGET-DURATION")
			if targetDurationStr == "" {
				logger.Warn("missing target-duration header")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			targetDuration, err := strconv.Atoi(targetDurationStr)
			if err != nil || targetDuration <= 0 {
				logger.Warn("invalid target-duration header", "value", targetDurationStr, "error", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			// Read init.mp4 body with size limit.
			r.Body = http.MaxBytesReader(w, r.Body, stream.MaxInitBytes)
			initData, err := io.ReadAll(r.Body)
			if err != nil {
				var maxBytesErr *http.MaxBytesError
				if errors.As(err, &maxBytesErr) {
					logger.Warn("init body too large", "limit", stream.MaxInitBytes)
					w.WriteHeader(http.StatusRequestEntityTooLarge)
				} else {
					logger.Warn("failed to read init body", "error", err)
					w.WriteHeader(http.StatusBadRequest)
				}
				return
			}
			if len(initData) == 0 {
				logger.Warn("empty init body")
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			// Parse buffer capacity headers.
			segmentCapStr := r.Header.Get("X-SL-SEGMENT-CAP")
			if segmentCapStr == "" {
				logger.Warn("missing segment-cap header")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			segmentCap, err := strconv.Atoi(segmentCapStr)
			if err != nil || segmentCap <= 0 {
				logger.Warn("invalid segment-cap header", "value", segmentCapStr, "error", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			segmentBytesStr := r.Header.Get("X-SL-SEGMENT-BYTES")
			if segmentBytesStr == "" {
				logger.Warn("missing segment-bytes header")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			segmentBytes, err := strconv.Atoi(segmentBytesStr)
			if err != nil || segmentBytes <= 0 {
				logger.Warn("invalid segment-bytes header", "value", segmentBytesStr, "error", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			totalSlots := int64(segmentCap) + int64(env.BUFFER_WORKING_SPACE)
			if totalSlots > math.MaxInt64/int64(segmentBytes) {
				logger.Warn("requested buffer overflows int64",
					"segmentCap", segmentCap,
					"segmentBytes", segmentBytes,
				)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			totalBufferBytes := totalSlots * int64(segmentBytes)
			if totalBufferBytes > env.STREAM_MAX_BUFFER_BYTES {
				logger.Warn("requested buffer exceeds maximum",
					"segmentCap", segmentCap,
					"segmentBytes", segmentBytes,
					"totalBufferBytes", totalBufferBytes,
					"maxBufferBytes", env.STREAM_MAX_BUFFER_BYTES,
				)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			backwardBufferSizeStr := r.Header.Get("X-SL-BACKWARD-BUFFER-SIZE")
			if backwardBufferSizeStr == "" {
				logger.Warn("missing backward-buffer-size header")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			backwardBufferSize, err := strconv.Atoi(backwardBufferSizeStr)
			if err != nil || backwardBufferSize <= 0 {
				logger.Warn("invalid backward-buffer-size header", "value", backwardBufferSizeStr, "error", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			meta := stream.Metadata{
				Bandwidth:          bandwidth,
				Codecs:             codecs,
				Width:              width,
				Height:             height,
				FrameRate:          framerate,
				TargetDurationSecs: targetDuration,
			}
			if err := store.Init(streamID, meta, initData, segmentCap, segmentBytes, backwardBufferSize, env.BUFFER_WORKING_SPACE, config.DefaultMediaWindowSize); err != nil {
				logger.Warn("invalid stream configuration", "error", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			logger.Info("stream initialized",
				"streamID", streamID,
				"bandwidth", bandwidth,
				"codecs", codecs,
				"resolution", resolutionStr,
				"framerate", framerate,
				"segmentCap", segmentCap,
				"segmentBytes", segmentBytes,
				"backwardBufferSize", backwardBufferSize,
			)

			w.WriteHeader(http.StatusCreated)
		})

		r.Post("/segment", func(w http.ResponseWriter, r *http.Request) {
			streamID := r.Context().Value(streamIDKey).(string)
			logger.Debug("handling stream segment upload request", "streamID", streamID)

			s := store.Get(streamID)
			if s == nil {
				logger.Warn("stream not found for segment upload", "streamID", streamID)
				w.WriteHeader(http.StatusNotFound)
				return
			}

			// Index is the segment sequence number from the transcoder.
			indexStr := r.Header.Get("X-SL-INDEX")
			if indexStr == "" {
				logger.Warn("missing index header")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			index, err := strconv.ParseUint(indexStr, 10, 32)
			if err != nil {
				logger.Warn("invalid index header", "value", indexStr, "error", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			// Timestamp is the UNIX timestamp in milliseconds indicating the start time of the segment.
			timestampStr := r.Header.Get("X-SL-TIMESTAMP")
			if timestampStr == "" {
				logger.Warn("missing timestamp header")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			timestampNum, err := strconv.ParseInt(timestampStr, 10, 64)
			if err != nil {
				logger.Warn("invalid timestamp header", "error", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			// Duration is the number of milliseconds that the segment covers.
			durationStr := r.Header.Get("X-SL-DURATION")
			if durationStr == "" {
				logger.Warn("missing duration header")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			durationMs, err := strconv.ParseUint(durationStr, 10, 32)
			if err != nil {
				logger.Warn("invalid duration header", "error", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if durationMs == 0 {
				logger.Warn("invalid duration header: zero duration", "duration", durationMs)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			// Acquire a buffer slot from the pool.
			buf, ok := s.AcquireSlot()
			if !ok {
				logger.Warn("buffer pool exhausted", "streamID", streamID)
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}

			// Limit request body to the pre-allocated slot capacity plus one
			// byte so the BufferSlot.ReadFrom overflow probe can detect
			// oversized bodies. Without the extra byte, MaxBytesReader
			// would silently block the probe read and mask the overflow.
			r.Body = http.MaxBytesReader(w, r.Body, int64(s.Metadata().SegmentByteCount)+1)

			// Read request body directly into the pre-allocated slot.
			n, err := buf.ReadFrom(r.Body)
			if err != nil {
				s.ReleaseSlot(buf)
				var maxBytesErr *http.MaxBytesError
				if errors.Is(err, pool.ErrOverflow) || errors.As(err, &maxBytesErr) {
					logger.Warn("segment body too large", "streamID", streamID)
					w.WriteHeader(http.StatusRequestEntityTooLarge)
				} else {
					logger.Warn("failed to read segment body", "error", err)
					w.WriteHeader(http.StatusBadRequest)
				}
				return
			}
			if n == 0 {
				s.ReleaseSlot(buf)
				logger.Warn("empty segment body")
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			if err := s.CommitSlot(uint32(index), buf, timestampNum, uint32(durationMs)); err != nil {
				s.ReleaseSlot(buf)
				if errors.Is(err, stream.ErrTimestampInPast) {
					logger.Warn("segment timestamp is in the past", "streamID", streamID, "index", index, "timestamp", timestampNum)
					w.WriteHeader(http.StatusUnprocessableEntity)
					return
				}
				if errors.Is(err, stream.ErrDuplicateIndex) {
					logger.Warn("duplicate segment index", "streamID", streamID, "index", index)
					w.WriteHeader(http.StatusConflict)
					return
				}
				if errors.Is(err, stream.ErrTimestampOrderViolation) {
					logger.Warn("segment timestamp order violation", "streamID", streamID, "index", index, "timestamp", timestampNum, "error", err)
					w.WriteHeader(http.StatusUnprocessableEntity)
					return
				}
				if errors.Is(err, stream.ErrBufferFull) {
					logger.Warn("buffer full", "streamID", streamID, "index", index)
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				logger.Error("failed to commit segment", "error", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			segLen, segCap := s.SegmentLoad()
			if segCap > 0 && segLen >= segCap*4/5 {
				leadMs := timestampNum - store.Clock().Now().UnixMilli()
				logger.Info("segment accepted near saturation",
					"streamID", streamID,
					"index", index,
					"segment_lead_ms", leadMs,
					"segments", segLen,
					"segment_cap", segCap,
				)
			}

			logger.Debug("segment uploaded", "streamID", streamID, "index", index, "timestamp", time.UnixMilli(timestampNum), "duration", durationMs, "size", n)
			w.WriteHeader(http.StatusCreated)
		})
	})

	return router
}
