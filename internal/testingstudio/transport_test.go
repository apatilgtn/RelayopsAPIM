package testingstudio

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestTrustedGatewayExactOrigin(t *testing.T) {
	for _, tc := range []struct {
		target string
		want   bool
	}{
		{"https://gateway.example/api", true}, {"https://gateway.example:443/api", true},
		{"https://gateway.example.attacker.com", false}, {"https://gateway.example:444", false},
		{"http://gateway.example", false}, {"https://user@gateway.example", false},
		{"http://localhost:9090", false}, {"http://localhost:8080/path", true},
	} {
		t.Run(tc.target, func(t *testing.T) {
			if got := IsTrustedGateway(tc.target, "https://gateway.example", "http://localhost:8080"); got != tc.want {
				t.Fatalf("trusted=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestTransportConnectsOnlyValidatedIP(t *testing.T) {
	for _, tc := range []struct {
		name, ip          string
		approved, allowed bool
	}{
		{"public", "203.0.113.7", false, true}, {"metadata", "169.254.169.254", true, false},
		{"private-unapproved", "10.0.0.2", false, false}, {"private-approved", "10.0.0.2", true, true},
		{"ipv6-metadata", "fd00:ec2::254", true, false}, {"unspecified", "0.0.0.0", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newSafeTransport(nil)
			if tc.approved {
				tr.approved = []string{"http://gateway.example:8080"}
			}
			lookups, dials := 0, 0
			tr.lookup = func(context.Context, string) ([]net.IPAddr, error) {
				lookups++
				return []net.IPAddr{{IP: net.ParseIP(tc.ip)}}, nil
			}
			tr.dial = func(_ context.Context, _, address string) (net.Conn, error) {
				dials++
				if address != net.JoinHostPort(tc.ip, "8080") {
					t.Fatalf("dialed unvalidated address %s", address)
				}
				a, b := net.Pipe()
				b.Close()
				return a, nil
			}
			u, _ := url.Parse("http://gateway.example:8080")
			c, err := tr.dialContext(context.WithValue(context.Background(), targetContextKey{}, u), "tcp", "gateway.example:8080")
			if c != nil {
				c.Close()
			}
			if (err == nil) != tc.allowed || lookups != 1 || (dials > 0) != tc.allowed {
				t.Fatalf("err=%v lookups=%d dials=%d", err, lookups, dials)
			}
		})
	}
}

func TestTransportLiveApprovedAndUnapprovedOrigin(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer s.Close()
	for _, approved := range []bool{false, true} {
		var origins []string
		if approved {
			origins = []string{s.URL}
		}
		tr := newSafeTransport(origins)
		defer tr.base.CloseIdleConnections()
		client := &http.Client{Transport: tr}
		resp, err := client.Get(s.URL)
		if approved && (err != nil || resp.StatusCode != 204) {
			t.Fatalf("approved live request: %v", err)
		}
		if !approved && err == nil {
			resp.Body.Close()
			t.Fatal("unapproved local request connected")
		}
		if resp != nil {
			resp.Body.Close()
		}
	}
}
