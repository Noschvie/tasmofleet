package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const checkTimeout = 8 * time.Second

// CheckIssuer runs OIDC discovery against issuer, the check behind the "test
// SSO" button of the system settings page. The host name is resolved once and
// only addresses allow accepts are dialled, redirects are not followed, and the
// answer is coarse (details go to the log), so the button is no general fetcher.
// No secret is sent.
func CheckIssuer(ctx context.Context, issuer string, allow func(netip.Addr) bool) (string, error) {
	u, err := url.Parse(issuer)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return "", errors.New("the issuer must be an http(s) URL without credentials")
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	client := &http.Client{
		Timeout:       checkTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				ip, err := pickAddr(ctx, host, allow)
				if err != nil {
					return nil, err
				}
				var d net.Dialer
				return d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			},
		},
	}
	p, err := oidc.NewProvider(oidc.ClientContext(ctx, client), issuer)
	if err != nil {
		slog.Warn("sso check failed", "host", u.Host, "err", err)
		return "", fmt.Errorf("no usable OIDC provider at %s (check the issuer URL; details are in the log)", u.Host)
	}
	ep := p.Endpoint()
	return fmt.Sprintf("discovery OK: authorization at %s, token endpoint at %s", hostOf(ep.AuthURL), hostOf(ep.TokenURL)), nil
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return "?"
}

// pickAddr resolves host once and returns the first address allow accepts.
func pickAddr(ctx context.Context, host string, allow func(netip.Addr) bool) (netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		if allow(ip.Unmap()) {
			return ip.Unmap(), nil
		}
		return netip.Addr{}, errors.New("address not allowed")
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return netip.Addr{}, err
	}
	for _, a := range addrs {
		if ip, ok := netip.AddrFromSlice(a.IP); ok && allow(ip.Unmap()) {
			return ip.Unmap(), nil
		}
	}
	return netip.Addr{}, errors.New("address not allowed")
}
