package web

import (
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/netaddr"
)

// deviceLink is where the "open the device's own web page" link points and
// what it shows.
type deviceLink struct {
	Href    string
	Display string
}

// deviceWebLink builds the link to a device's own web UI from its stored
// address and port. http is the default; https only for port 443. Default
// ports (80 for http, 443 for https) are left out of both the URL and the
// text, other ports are shown, IPv6 literals get brackets. The URL never has
// user information (TasmoAdmin leaked user:pass@ links), and nothing here
// makes a request: the link only repeats what is stored. Host and port
// formatting is internal/netaddr (the one shared implementation); this file
// only decides whether something may become a link at all. It returns nil when
// the stored address is not a plain host name or IP address, so a malformed
// value never turns into a clickable link.
func deviceWebLink(address string, port int) *deviceLink {
	host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(address), "["), "]")
	if !plainHost(host) || port < 0 || port > 65535 {
		return nil
	}
	scheme := "http"
	if port == 443 {
		scheme = "https"
	}
	return &deviceLink{Href: netaddr.URL(scheme, host, port, ""), Display: netaddr.Display(host, port, scheme)}
}

// urlLink makes a link out of a full URL shown in the UI (public URL, OIDC
// issuer, firmware base URL). Only http and https become links, with the same
// rules as deviceWebLink: default ports are dropped, the host must be plain,
// and user information, query and fragment are refused (a query can carry a
// token, and a link must never leak or hide one). Everything else returns nil
// and is shown as text through urlText.
func urlLink(raw string) *deviceLink {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return nil
	}
	port := 0
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return nil
		}
		port = n
	}
	// Reuse the device rule for host and port, then put the scheme the URL
	// asked for (not the port heuristic) and the path back.
	host := u.Hostname()
	if !plainHost(host) {
		return nil
	}
	path := u.EscapedPath()
	return &deviceLink{Href: netaddr.URL(u.Scheme, host, port, path), Display: netaddr.Display(host, port, u.Scheme) + strings.TrimSuffix(path, "/")}
}

// urlText is what a URL that is not a link looks like: user information is
// removed so a mqtt://user:pass@host value can never show its credentials.
func urlText(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

// plainHost accepts IP addresses (without zone) and DNS names made of letters,
// digits, dots, hyphens and underscores. Anything else (user info, paths,
// spaces, ports in the address field) is refused.
func plainHost(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	if ip := net.ParseIP(h); ip != nil {
		return !strings.Contains(h, "%")
	}
	for _, c := range h {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
