package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dskiff/streamloom/pkg/clock"
	"github.com/dskiff/streamloom/pkg/watcher"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
)

// newWatcherTestRouter mounts RecordWatcher inside a {streamID} route group so
// chi.URLParam resolves when the middleware runs, mirroring production wiring
// (params are matched after any root-level middleware).
func newWatcherTestRouter(tr *watcher.Tracker, exists func(string) bool) *chi.Mux {
	r := chi.NewRouter()
	r.Route("/stream/{streamID}", func(r chi.Router) {
		r.Use(RecordWatcher(tr, exists))
		r.Get("/media.m3u8", func(w http.ResponseWriter, r *http.Request) {})
	})
	return r
}

func TestRecordWatcher_RecordsWhenStreamExists(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	tr := watcher.NewTracker(clk)
	var gotID string
	router := newWatcherTestRouter(tr, func(id string) bool { gotID = id; return true })

	req := httptest.NewRequest(http.MethodGet, "/stream/s1/media.m3u8", nil)
	req.RemoteAddr = "203.0.113.7:1234"
	router.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, "s1", gotID, "predicate must receive the URL stream ID")
	assert.Equal(t, 1, tr.ActiveCount("s1", watcher.MaxWindowMs))
}

func TestRecordWatcher_SkipsWhenStreamMissing(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	tr := watcher.NewTracker(clk)
	router := newWatcherTestRouter(tr, func(string) bool { return false })

	req := httptest.NewRequest(http.MethodGet, "/stream/ghost/media.m3u8", nil)
	req.RemoteAddr = "203.0.113.7:1234"
	router.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, 0, tr.ActiveCount("ghost", watcher.MaxWindowMs),
		"a request to a non-existent stream must not be recorded")
}

func TestRecordWatcher_NilPredicatePanics(t *testing.T) {
	clk := clock.NewMock(time.UnixMilli(1000))
	tr := watcher.NewTracker(clk)
	assert.Panics(t, func() { RecordWatcher(tr, nil) },
		"a nil streamExists predicate must panic at construction, not on first request")
}

func TestExtractIP(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want string
	}{
		{"ip with port", "192.168.1.1:8080", "192.168.1.1"},
		{"bare ip", "192.168.1.1", "192.168.1.1"},
		{"ipv6 with port", "[::1]:8080", "::1"},
		{"bare ipv6", "::1", "::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractIP(tt.addr))
		})
	}
}
