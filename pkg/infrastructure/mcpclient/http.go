package mcpclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func oauthHTTPClient(endpoint string) *http.Client {
	origin, _ := url.Parse(endpoint)
	local := loopbackHost(origin.Hostname())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	direct := transport.Clone()
	direct.Proxy = nil
	// A proxy resolves the destination itself. Apply IP checks only to direct connections.
	direct.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		allowLoopback := local && address == dialAddress(origin)
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second,
			Control: func(_, destination string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(destination)
				if err != nil {
					return err
				}
				ip, err := netip.ParseAddr(host)
				if err != nil || !publicIP(ip) && (!allowLoopback || !ip.IsLoopback()) {
					return errors.New("MCP HTTP destination is not public")
				}
				return nil
			},
		}
		return dialer.DialContext(ctx, network, address)
	}
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("MCP HTTP redirect limit exceeded")
		}
		if via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errors.New("MCP HTTP redirect downgrades HTTPS")
		}
		return nil
	},
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if !validURL(r.URL.String()) || loopbackHost(r.URL.Hostname()) && (!local || dialAddress(r.URL) != dialAddress(origin)) {
				return nil, errors.New("unsafe MCP HTTP URL")
			}
			proxy, err := transport.Proxy(r)
			if err != nil {
				return nil, err
			}
			if proxy != nil {
				return transport.RoundTrip(r)
			}
			return direct.RoundTrip(r)
		}),
	}
}

func dialAddress(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

func loopbackHost(host string) bool {
	ip, err := netip.ParseAddr(host)
	return host == "localhost" || err == nil && ip.Unmap().IsLoopback()
}

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() {
		return false
	}
	for _, reserved := range nonPublicRanges {
		if reserved.Contains(ip) {
			return false
		}
	}
	return true
}

var nonPublicRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
}
