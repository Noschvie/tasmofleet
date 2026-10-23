// Package netaddr is the one place that formats network addresses for
// humans and links. Displayed addresses never show the default port of their
// scheme (:80 for http, :443 for https); other ports stay, and IPv6 hosts are
// bracketed. Error texts, audit details, job errors and the web UI all use it.
//
// It is for display only. Dialing and map keys keep using net.JoinHostPort.
package netaddr

import (
	"strconv"
	"strings"
)

// defaultPort returns the default port of scheme; an empty scheme means http.
func defaultPort(scheme string) int {
	switch strings.ToLower(scheme) {
	case "https", "wss":
		return 443
	default:
		return 80
	}
}

// Display returns host[:port] for humans. The port is omitted when it is 0
// or the default for scheme. IPv6 literals are put in brackets.
func Display(host string, port int, scheme string) string {
	h := host
	if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
		h = "[" + h + "]"
	}
	if port == 0 || port == defaultPort(scheme) {
		return h
	}
	return h + ":" + strconv.Itoa(port)
}

// URL returns scheme://Display(host, port, scheme)path. A path without a
// leading slash gets one; an empty path stays empty.
func URL(scheme, host string, port int, path string) string {
	if scheme == "" {
		scheme = "http"
	}
	if path != "" && !strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "?") {
		path = "/" + path
	}
	return scheme + "://" + Display(host, port, scheme) + path
}
