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

func TestTrustedRealIP_IgnoresTrueClientIP(t *testing.T) {
	trusted := []*net.IPNet{mustParseCIDR("10.0.0.0/8")}
	var gotRemoteAddr, gotTCIP string
	handler := TrustedRealIP(trusted)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRemoteAddr = r.RemoteAddr
		gotTCIP = r.Header.Get("True-Client-IP")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:1234"                  // trusted proxy
	req.Header.Set("True-Client-IP", "203.0.113.50")  // spoof attempt
	req.Header.Set("X-Forwarded-For", "198.51.100.7") // real client from proxy
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, "198.51.100.7", gotRemoteAddr, "True-Client-IP must not override the forwarded client IP")
	assert.Empty(t, gotTCIP, "True-Client-IP must be stripped")
}

func TestTrustedRealIP_StripsTrueClientIPWhenUntrusted(t *testing.T) {
	trusted := []*net.IPNet{mustParseCIDR("10.0.0.0/8")}
	var gotRemoteAddr, gotTCIP string
	handler := TrustedRealIP(trusted)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRemoteAddr = r.RemoteAddr
		gotTCIP = r.Header.Get("True-Client-IP")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.7:5678" // untrusted direct client
	req.Header.Set("True-Client-IP", "10.0.0.99")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, "203.0.113.7:5678", gotRemoteAddr)
	assert.Empty(t, gotTCIP, "True-Client-IP must be stripped on the untrusted path too")
}

func TestTrustedRealIP_UsesRightmostForwardedEntry(t *testing.T) {
	trusted := []*net.IPNet{mustParseCIDR("10.0.0.0/8")}
	var gotRemoteAddr string
	handler := TrustedRealIP(trusted)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRemoteAddr = r.RemoteAddr
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	// Client prepends a fake leftmost entry; the proxy appends the true peer.
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 198.51.100.7")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, "198.51.100.7", gotRemoteAddr,
		"must use the rightmost (proxy-appended) entry, not the spoofable leftmost")
}

func TestTrustedRealIP_IgnoresXRealIP(t *testing.T) {
	trusted := []*net.IPNet{mustParseCIDR("10.0.0.0/8")}
	var gotRemoteAddr, gotXRealIP string
	handler := TrustedRealIP(trusted)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRemoteAddr = r.RemoteAddr
		gotXRealIP = r.Header.Get("X-Real-IP")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Real-IP", "203.0.113.50") // must be ignored
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// With no X-Forwarded-For, RemoteAddr stays the trusted peer; X-Real-IP is
	// never consulted and is stripped.
	assert.Equal(t, "10.0.0.1:1234", gotRemoteAddr)
	assert.Empty(t, gotXRealIP, "X-Real-IP must be stripped and not used")
}

func TestTrustedRealIP_MalformedForwardedKeepsPeer(t *testing.T) {
	trusted := []*net.IPNet{mustParseCIDR("10.0.0.0/8")}
	var gotRemoteAddr string
	handler := TrustedRealIP(trusted)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRemoteAddr = r.RemoteAddr
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "not-an-ip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, "10.0.0.1:1234", gotRemoteAddr,
		"a malformed forwarded entry must fall back to the trusted peer")
}

func TestRightmostUntrustedIP(t *testing.T) {
	trusted := []*net.IPNet{
		mustParseCIDR("10.0.0.0/8"),
		mustParseCIDR("172.16.0.0/12"),
	}

	tests := []struct {
		name string
		xff  string
		want string
	}{
		{"empty", "", ""},
		{"single client", "203.0.113.9", "203.0.113.9"},
		{"client then trusted hop", "203.0.113.9, 10.0.0.2", "203.0.113.9"},
		{"attacker-prepended, real appended", "1.2.3.4, 203.0.113.9", "203.0.113.9"},
		{"spoofed trusted-looking left, real right", "10.9.9.9, 203.0.113.9", "203.0.113.9"},
		{"multiple trusted hops skipped", "203.0.113.9, 172.16.0.1, 10.0.0.2", "203.0.113.9"},
		{"all entries trusted", "10.0.0.2, 172.16.0.1", ""},
		{"malformed rightmost stops walk", "203.0.113.9, garbage", ""},
		{"whitespace tolerated", "203.0.113.9 , 10.0.0.2", "203.0.113.9"},
		{"ipv6 client", "2001:db8::1, 10.0.0.2", "2001:db8::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, rightmostUntrustedIP(tt.xff, trusted))
		})
	}
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
