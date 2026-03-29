package routes

import (
	"log/slog"
	"net/http"

	"github.com/dskiff/streamloom/pkg/config"
	slogchi "github.com/samber/slog-chi"
)

// requestLogMiddleware returns a slogchi middleware configured for request
// logging. When requestLogger is non-nil, logs are written at Info level to
// it; otherwise request logging is disabled.
func requestLogMiddleware(l *slog.Logger, requestLogger *slog.Logger) func(next http.Handler) http.Handler {
	requestLogLevel := config.LOG_LEVEL_DISABLED
	rl := l
	if requestLogger != nil {
		requestLogLevel = slog.LevelInfo
		rl = requestLogger
	}
	return slogchi.NewWithConfig(rl, slogchi.Config{
		DefaultLevel:     requestLogLevel,
		ClientErrorLevel: slog.LevelWarn,
		ServerErrorLevel: slog.LevelError,

		WithUserAgent:      false,
		WithRequestID:      true,
		WithRequestBody:    false,
		WithRequestHeader:  false,
		WithResponseBody:   false,
		WithResponseHeader: false,
		WithSpanID:         false,
		WithTraceID:        false,
		Filters:            []slogchi.Filter{},
	})
}
