package testingstudio

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type targetContextKey struct{}

// Resolve once, validate every answer, then connect to the numeric address.
// TLS still uses the original hostname through http.Transport.
type safeTransport struct {
	base     *http.Transport
	approved []string
	lookup   func(context.Context, string) ([]net.IPAddr, error)
	dial     func(context.Context, string, string) (net.Conn, error)
}

func newSafeTransport(approved []string) *safeTransport {
	t := &safeTransport{approved: approved, lookup: net.DefaultResolver.LookupIPAddr,
		dial: (&net.Dialer{Timeout: 10 * time.Second}).DialContext}
	t.base = &http.Transport{DialContext: t.dialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second}
	return t
}

func (t *safeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if _, err := ValidateGatewayTarget(r.URL.String()); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(r.Clone(context.WithValue(r.Context(), targetContextKey{}, r.URL)))
}

func (t *safeTransport) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	u, ok := ctx.Value(targetContextKey{}).(*url.URL)
	if !ok || !strings.EqualFold(host, u.Hostname()) || port != originPort(u) {
		return nil, fmt.Errorf("unexpected test destination")
	}
	ips, err := t.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("test destination has no addresses")
	}
	trusted := IsTrustedGateway(u.String(), t.approved...)
	for _, addr := range ips {
		ip := addr.IP
		if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.Equal(net.ParseIP("fd00:ec2::254")) || ((ip.IsPrivate() || ip.IsLoopback()) && !trusted) {
			return nil, fmt.Errorf("test destination address is not permitted")
		}
	}
	var last error
	for _, addr := range ips {
		conn, err := t.dial(ctx, network, net.JoinHostPort(addr.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}
