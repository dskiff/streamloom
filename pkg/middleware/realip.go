package middleware

import (
	"net"
	"net/http"
	"strings"
)

// TrustedRealIP returns a middleware that resolves the real client IP into
// r.RemoteAddr according to whether the direct connection comes from a trusted
// reverse proxy.
//
// Header trust model:
//
//   - True-Client-IP and X-Real-IP are NEVER honored and are stripped on every
//     request. A reverse proxy with a default configuration (e.g. Caddy's
//     reverse_proxy) forwards these client-supplied headers through untouched,
//     so trusting them would let any client spoof its source IP by simply
//     setting a header. Because the resolved IP keys an in-memory
//     active-watcher map, that would also let a single client mint unbounded
//     distinct map entries.
//
//   - X-Forwarded-For is consulted ONLY when the direct connection
//     (r.RemoteAddr) is itself a trusted proxy. A trusted proxy appends the
//     immediate peer to X-Forwarded-For, so the real client is the RIGHTMOST
//     entry that is not itself a trusted proxy; entries further left can be
//     forged by the client and are skipped. The header is then stripped on
//     every path — its spoofable left-hand entries must never reach a
//     downstream consumer — so the resolved RemoteAddr is the single source of
//     client-IP truth.
//
// When trustedNets is empty, no proxy is trusted and the direct connection IP
// is always used (forwarded headers are stripped).
//
// Note: this middleware assumes TCP connections where RemoteAddr is
// "host:port". For a trusted connection whose X-Forwarded-For yields no usable
// client entry, RemoteAddr is left unchanged (the trusted peer's address)
// rather than guessed.
func TrustedRealIP(trustedNets []*net.IPNet) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// None of these forwarding headers are propagated downstream: the
			// authoritative client IP is resolved into RemoteAddr here, and
			// every one of them is client-forgeable under a default-configured
			// proxy. Capture X-Forwarded-For for use on the trusted path, then
			// strip all three up front so no later middleware or handler can be
			// tricked into trusting their (spoofable) contents.
			xff := r.Header.Get("X-Forwarded-For")
			r.Header.Del("X-Forwarded-For")
			r.Header.Del("True-Client-IP")
			r.Header.Del("X-Real-IP")

			if !IsTrusted(r.RemoteAddr, trustedNets) {
				// Direct (untrusted) client: it cannot speak for any other
				// host, so the forwarding chain it supplied was just discarded.
				next.ServeHTTP(w, r)
				return
			}

			if clientIP := rightmostUntrustedIP(xff, trustedNets); clientIP != "" {
				r.RemoteAddr = clientIP
			}
			// Otherwise keep the trusted peer's RemoteAddr rather than inventing
			// one from an empty or fully-trusted chain.
			next.ServeHTTP(w, r)
		})
	}
}

// rightmostUntrustedIP walks an X-Forwarded-For value from right to left and
// returns the first entry that is a valid IP not contained in trustedNets —
// the nearest hop that is not one of our own trusted proxies, i.e. the real
// client. It returns "" when the header is empty, when every entry is a
// trusted proxy, or when it reaches a malformed entry before finding a client.
//
// Walking from the right is what makes this spoof-resistant: a trusted proxy
// appends the immediate peer, so the genuine entries are on the right and a
// client can only inject additional entries on the left. Stopping at the first
// malformed token is safe for the same reason — a malformed entry can only
// have been injected by the client to the left of the genuine appended hops.
func rightmostUntrustedIP(xff string, trustedNets []*net.IPNet) string {
	if xff == "" {
		return ""
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		entry := strings.TrimSpace(parts[i])
		ip := net.ParseIP(entry)
		if ip == nil {
			return ""
		}
		if ipInNets(ip, trustedNets) {
			continue
		}
		return entry
	}
	return ""
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
	return ipInNets(ip, trustedNets)
}

// ipInNets reports whether ip is contained in any of nets.
func ipInNets(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
