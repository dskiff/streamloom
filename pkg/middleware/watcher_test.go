package middleware

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
