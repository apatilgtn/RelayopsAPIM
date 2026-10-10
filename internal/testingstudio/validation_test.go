package testingstudio

import (
	"testing"

	"github.com/relayops/apim/internal/store"
)

func TestValidateGatewayTarget(t *testing.T) {
	validTargets := []string{
		"http://127.0.0.1:8080",
		"http://localhost:8080",
		"https://gateway.internal.relayops.net:8443",
	}

	for _, target := range validTargets {
		u, err := ValidateGatewayTarget(target)
		if err != nil || u == nil {
			t.Errorf("Expected valid gateway target for %s, got error: %v", target, err)
		}
	}

	blockedTargets := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://169.254.1.1/internal",
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://localhost:5432",
		"http://localhost:6379",
		"http://127.0.0.1:22",
		"ftp://localhost:21",
		"",
	}

	for _, target := range blockedTargets {
		_, err := ValidateGatewayTarget(target)
		if err == nil {
			t.Errorf("Expected blocked gateway target for %s, but validation succeeded", target)
		}
	}
}

func TestValidateSuiteDefinition(t *testing.T) {
	validDef := store.SuiteDefinition{
		Name: "Sample Suite",
		Requests: []store.RequestDef{
			{
				ID:     "req-1",
				Name:   "Get Status",
				Method: "GET",
				Path:   "/status",
				Assertions: []store.AssertionDef{
					{Type: "status_code", Expected: "200"},
				},
			},
		},
	}

	if err := ValidateSuiteDefinition(validDef); err != nil {
		t.Fatalf("Expected valid suite definition, got error: %v", err)
	}

	traversalDef := validDef
	traversalDef.Requests[0].Path = "/../../etc/passwd"
	if err := ValidateSuiteDefinition(traversalDef); err == nil {
		t.Errorf("Expected traversal path to be rejected")
	}

	absoluteURLDef := validDef
	absoluteURLDef.Requests[0].Path = "http://evil.com/leak"
	if err := ValidateSuiteDefinition(absoluteURLDef); err == nil {
		t.Errorf("Expected absolute URL path to be rejected")
	}
}
