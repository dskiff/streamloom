package middleware

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func mustParseCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

func TestTrustedRealIP_TrustedUsesForwarded(t *testing.T) {
	trusted := []*net.IPNet{mustParseCIDR("10.0.0.0/8")}
	var gotRemoteAddr string
	handler := TrustedRealIP(trusted)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRemoteAddr = r.RemoteAddr
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.50")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// chi's RealIP middleware rewrites RemoteAddr from X-Forwarded-For.
	assert.Equal(t, "203.0.113.50", gotRemoteAddr)
}

func TestTrustedRealIP_UntrustedStripsForwarded(t *testing.T) {
	trusted := []*net.IPNet{mustParseCIDR("10.0.0.0/8")}
	var gotXFF, gotXRealIP, gotRemoteAddr string
	handler := TrustedRealIP(trusted)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFF = r.Header.Get("X-Forwarded-For")
		gotXRealIP = r.Header.Get("X-Real-IP")
		gotRemoteAddr = r.RemoteAddr
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.1.1:5678"
	req.Header.Set("X-Forwarded-For", "203.0.113.50")
	req.Header.Set("X-Real-IP", "203.0.113.50")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Empty(t, gotXFF, "X-Forwarded-For should be stripped for untrusted origin")
	assert.Empty(t, gotXRealIP, "X-Real-IP should be stripped for untrusted origin")
	assert.Equal(t, "192.168.1.1:5678", gotRemoteAddr)
}

func TestTrustedRealIP_EmptyTrustedNeverTrusts(t *testing.T) {
	var gotXFF string
	handler := TrustedRealIP(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFF = r.Header.Get("X-Forwarded-For")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.50")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Empty(t, gotXFF, "forwarded headers should be stripped when no trusted proxies configured")
}

func TestIsTrusted(t *testing.T) {
	nets := []*net.IPNet{
		mustParseCIDR("10.0.0.0/8"),
		mustParseCIDR("172.16.0.0/12"),
	}

	tests := []struct {
		addr string
		want bool
	}{
		{"10.0.0.1:8080", true},
		{"10.255.255.255:80", true},
		{"172.16.0.1:80", true},
		{"172.31.255.255:80", true},
		{"192.168.1.1:80", false},
		{"8.8.8.8:53", false},
		{"invalid", false},
		{"", false},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, IsTrusted(tt.addr, nets), "IsTrusted(%q)", tt.addr)
	}
}

func TestIsTrusted_EmptyNets(t *testing.T) {
	assert.False(t, IsTrusted("10.0.0.1:80", nil))
	assert.False(t, IsTrusted("10.0.0.1:80", []*net.IPNet{}))
}
