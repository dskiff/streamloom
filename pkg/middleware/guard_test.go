package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dskiff/streamloom/pkg/pool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnrecoverableGuardExitsOnPoolPanic(t *testing.T) {
	var exitCode int
	exitFunc = func(code int) {
		exitCode = code
		// Panic to abort the handler; the test defers a recover.
		panic("exit called")
	}
	t.Cleanup(func() {
		exitFunc = nil // restored by other tests or next init
	})

	handler := UnrecoverableGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(pool.Unrecoverable{Msg: "double-free"})
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()

	func() {
		defer func() { recover() }()
		handler.ServeHTTP(rr, req)
	}()

	assert.Equal(t, 70, exitCode)
}

func TestUnrecoverableGuardRepanicsOtherPanics(t *testing.T) {
	handler := UnrecoverableGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("some other error")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		handler.ServeHTTP(rr, req)
	}()

	require.NotNil(t, recovered)
	msg, ok := recovered.(string)
	require.True(t, ok)
	assert.Equal(t, "some other error", msg)
}

func TestUnrecoverableGuardPassesNormalRequests(t *testing.T) {
	handler := UnrecoverableGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
}
