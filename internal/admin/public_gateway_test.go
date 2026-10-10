package admin

import (
	"testing"

	"github.com/relayops/apim/internal/store"
)

// API consumers must see the public gateway address in published references,
// not the control plane's local listener.
func TestPublishedReferenceUsesPublicGatewayURL(t *testing.T) {
	api := store.API{Name: "orders", BasePath: "/orders/"}
	server := func(s *Server) string {
		doc := s.synthesizeOpenAPISpec(api)
		return doc["servers"].([]map[string]any)[0]["url"].(string)
	}

	s := &Server{gatewayURL: "http://127.0.0.1:8080"}
	WithPublicGatewayURL("https://api.example.com/")(s)
	if got := server(s); got != "https://api.example.com/orders" {
		t.Fatalf("server url = %q, want the public gateway", got)
	}
	if got := s.consumerGatewayURL(); got != "https://api.example.com" {
		t.Fatalf("consumer gateway url = %q", got)
	}

	// Without a public address, the local gateway is used as before.
	local := &Server{gatewayURL: "http://127.0.0.1:8080"}
	if got := server(local); got != "http://127.0.0.1:8080/orders" {
		t.Fatalf("fallback server url = %q", got)
	}
	if got := (&Server{}).consumerGatewayURL(); got != "http://localhost:8080" {
		t.Fatalf("default = %q", got)
	}
}
