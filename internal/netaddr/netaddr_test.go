package netaddr

import "testing"

// Why: users should read "plug.example.internal", not "plug.example.internal:80".
// A non-default port is information (a device behind a port forward) and must
// survive, and a bare IPv6 literal is ambiguous without brackets.
func TestDisplay(t *testing.T) {
	tests := []struct {
		host   string
		port   int
		scheme string
		want   string
	}{
		{"plug.example.internal", 80, "http", "plug.example.internal"},
		{"plug.example.internal", 0, "http", "plug.example.internal"},
		{"plug.example.internal", 80, "", "plug.example.internal"},
		{"plug.example.internal", 8080, "http", "plug.example.internal:8080"},
		{"plug.example.internal", 443, "http", "plug.example.internal:443"}, // 443 is not http's default
		{"plug.example.internal", 443, "https", "plug.example.internal"},
		{"plug.example.internal", 80, "https", "plug.example.internal:80"},
		{"plug.example.internal", 8443, "HTTPS", "plug.example.internal:8443"},
		{"172.31.13.28", 80, "http", "172.31.13.28"},
		{"172.31.13.28", 81, "http", "172.31.13.28:81"},
		{"fd00::5", 80, "http", "[fd00::5]"},
		{"fd00::5", 8080, "http", "[fd00::5]:8080"},
		{"[fd00::5]", 8080, "http", "[fd00::5]:8080"}, // already bracketed
		{"fe80::1%eth0", 0, "http", "[fe80::1%eth0]"},
	}
	for _, tc := range tests {
		if got := Display(tc.host, tc.port, tc.scheme); got != tc.want {
			t.Errorf("Display(%q, %d, %q) = %q, want %q", tc.host, tc.port, tc.scheme, got, tc.want)
		}
	}
}

func TestURL(t *testing.T) {
	tests := []struct {
		scheme, host string
		port         int
		path, want   string
	}{
		{"http", "plug.example.internal", 80, "", "http://plug.example.internal"},
		{"http", "plug.example.internal", 80, "/", "http://plug.example.internal/"},
		{"http", "plug.example.internal", 8080, "/cm?cmnd=Power", "http://plug.example.internal:8080/cm?cmnd=Power"},
		{"", "172.31.13.28", 0, "up", "http://172.31.13.28/up"},
		{"https", "grafana.example", 443, "/d/x", "https://grafana.example/d/x"},
		{"https", "grafana.example", 80, "/", "https://grafana.example:80/"},
		{"http", "fd00::5", 8080, "/", "http://[fd00::5]:8080/"},
		{"http", "fd00::5", 80, "?a=b", "http://[fd00::5]?a=b"},
	}
	for _, tc := range tests {
		if got := URL(tc.scheme, tc.host, tc.port, tc.path); got != tc.want {
			t.Errorf("URL(%q, %q, %d, %q) = %q, want %q", tc.scheme, tc.host, tc.port, tc.path, got, tc.want)
		}
	}
}
