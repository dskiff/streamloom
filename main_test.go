package main

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListenAddr(t *testing.T) {
	tests := []struct {
		name string
		host string
		port int
		want string
	}{
		{name: "ipv4", host: "127.0.0.1", port: 8080, want: "127.0.0.1:8080"},
		{name: "ipv4 unspecified", host: "0.0.0.0", port: 80, want: "0.0.0.0:80"},
		// Regression: SL_BIND_ADDR=::1 must produce a bracketed address.
		// fmt.Sprintf("%s:%d", ...) yielded "::1:8080", which net.Listen
		// rejects with "too many colons in address".
		{name: "ipv6 loopback", host: "::1", port: 8080, want: "[::1]:8080"},
		{name: "ipv6 unspecified", host: "::", port: 9090, want: "[::]:9090"},
		{name: "ipv6 full", host: "2001:db8::1", port: 443, want: "[2001:db8::1]:443"},
		// Empty host (production default before a SL_BIND_ADDR override)
		// binds all interfaces and must stay ":<port>".
		{name: "empty host", host: "", port: 8080, want: ":8080"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, listenAddr(tt.host, tt.port))
		})
	}
}

// TestListenAddr_AcceptedByNetListen proves the produced address actually
// boots a listener for every host the config layer blesses, closing the
// loop on the accepted-config-that-can't-run bug. Port 0 picks an
// ephemeral port so the test never collides with a real service.
func TestListenAddr_AcceptedByNetListen(t *testing.T) {
	hosts := []string{"127.0.0.1", "::1", ""}
	for _, host := range hosts {
		t.Run("host="+host, func(t *testing.T) {
			ln, err := net.Listen("tcp", listenAddr(host, 0))
			if err != nil {
				// IPv6 loopback may be unavailable in some sandboxes; only
				// skip in that narrow case rather than masking real breakage.
				if host == "::1" {
					t.Skipf("IPv6 loopback unavailable: %v", err)
				}
				require.NoError(t, err)
			}
			require.NoError(t, ln.Close())
		})
	}
}
