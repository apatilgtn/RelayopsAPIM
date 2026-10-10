package admin

import (
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/relayops/apim/internal/license"
)

// Option configures the control-plane server without changing existing callers.
type Option func(*Server)

// WithGatewayAddress connects the portal workbench to the actual data-plane listener.
// Wildcard bind addresses are converted to loopback for requests from this process.
func WithGatewayAddress(address string) Option {
	return func(s *Server) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return
		}
		switch host {
		case "", "0.0.0.0":
			host = "127.0.0.1"
		case "::":
			host = "::1"
		}
		s.gatewayURL = "http://" + net.JoinHostPort(host, port)
	}
}

// WithGatewayURL points the workbench, MCP and Test Studio at a gateway that is
// not this process's proxy listener (split deployments). Empty keeps the default.
func WithGatewayURL(u string) Option {
	return func(s *Server) {
		if u != "" {
			s.gatewayURL = u
		}
	}
}

// WithRunnerSecret configures the internal runner token for authorized Test Studio gateway requests.
func WithRunnerSecret(secret string) Option {
	return func(s *Server) {
		s.runnerToken = secret
	}
}

// WithLicense attaches invoice-edition entitlements used by /api/auth/me and tenant caps.
func WithLicense(e license.Entitlement) Option {
	return func(s *Server) {
		s.license = e
	}
}

// Portal assets are served from the embedded product bundle, independently of admin UI assets.
func (s *Server) servePortalAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	contentType := map[string]string{"portal.css": "text/css; charset=utf-8", "portal.js": "text/javascript; charset=utf-8"}[name]
	if contentType == "" {
		http.NotFound(w, r)
		return
	}
	content, err := fs.ReadFile(s.static, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(content)
}

func validPortalRequest(method, path string) bool {
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
	default:
		return false
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "\\\r\n#") {
		return false
	}
	u, err := url.ParseRequestURI(path)
	return err == nil && !u.IsAbs() && u.Host == ""
}
