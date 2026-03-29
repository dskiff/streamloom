package middleware

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/dskiff/streamloom/pkg/pool"
)

// exitFunc is the function called to terminate the process on unrecoverable
// pool errors. It is a variable so tests can replace it.
var exitFunc = os.Exit

// UnrecoverableGuard is a middleware that intercepts pool.Unrecoverable panics
// and terminates the process. It must be placed inside (after) middleware.Recoverer
// in the Use chain so that unrecoverable panics never reach Recoverer.
func UnrecoverableGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if u, ok := v.(pool.Unrecoverable); ok {
					slog.Error("unrecoverable pool error, terminating", "error", u.Msg)
					exitFunc(70) // EX_SOFTWARE
				}
				panic(v) // re-panic; let Recoverer handle it
			}
		}()
		next.ServeHTTP(w, r)
	})
}
