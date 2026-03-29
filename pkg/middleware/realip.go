package middleware

import (
	"net"
	"net/http"

	chimw "github.com/go-chi/chi/v5/middleware"
)

// TrustedRealIP returns a middleware that conditionally trusts X-Forwarded-For
// and X-Real-IP headers based on the client's IP address. If the direct
// connection IP falls within any of the trusted CIDR ranges, chi's
// middleware.RealIP is used to extract the real client IP. Otherwise, the
// forwarded headers are stripped to prevent spoofing.
//
// When trustedNets is empty, forwarded headers are never trusted.
//
// Note: this middleware assumes TCP connections where RemoteAddr is "host:port".
// Unix socket listeners are not supported; RemoteAddr would not be a valid
// host:port, so IsTrusted would always return false and forwarded headers would
// be unconditionally stripped.
func TrustedRealIP(trustedNets []*net.IPNet) func(http.Handler) http.Handler {
	realIP := chimw.RealIP
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if IsTrusted(r.RemoteAddr, trustedNets) {
				realIP(next).ServeHTTP(w, r)
				return
			}
			// Untrusted origin: strip forwarded headers to prevent spoofing.
			r.Header.Del("X-Forwarded-For")
			r.Header.Del("X-Real-IP")
			next.ServeHTTP(w, r)
		})
	}
}

// IsTrusted checks whether the host portion of remoteAddr falls within any of
// the given CIDR ranges. Returns false if trustedNets is empty or the address
// cannot be parsed.
func IsTrusted(remoteAddr string, trustedNets []*net.IPNet) bool {
	if len(trustedNets) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// remoteAddr might be a bare IP (no port).
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range trustedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
