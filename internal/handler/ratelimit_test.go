package handler

import (
	"net/http/httptest"
	"testing"
)

func TestClientKey(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		header     string
		headerIP   string
		want       string
	}{
		{"ipv4 connection", "203.0.113.7:51234", "", "", "203.0.113.7"},
		{"ipv6 grouped by /64", "[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:443", "", "", "2001:db8:1:2::/64"},
		{"ipv4-mapped ipv6 treated as ipv4", "[::ffff:203.0.113.7]:443", "", "", "203.0.113.7"},
		{"proxy header preferred", "10.0.0.1:443", "Fly-Client-IP", "198.51.100.9", "198.51.100.9"},
		{"proxy header ipv6 grouped", "10.0.0.1:443", "Fly-Client-IP", "2001:db8:1:2::5", "2001:db8:1:2::/64"},
		{"missing proxy header falls back", "203.0.113.7:51234", "Fly-Client-IP", "", "203.0.113.7"},
		{"header ignored when not configured", "203.0.113.7:51234", "", "198.51.100.9", "203.0.113.7"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/shorten", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.headerIP != "" {
				r.Header.Set("Fly-Client-IP", tt.headerIP)
			}

			if got := clientKey(r, tt.header); got != tt.want {
				t.Errorf("clientKey() = %q, want %q", got, tt.want)
			}
		})
	}
}
