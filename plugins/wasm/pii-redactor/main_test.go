package main

import (
	"strings"
	"testing"

	pt "github.com/relayops/apim/plugins/wasm/internal/plugintest"
)

func TestPIIRedactor(t *testing.T) {
	body := `{"prompt":"Customer jane.doe@example.com paid with 4111 1111 1111 1111 (order 1234 5678 9012 3456), SSN 123-45-6789, call +61 412 345 678 from 192.168.10.20"}`
	res := pt.Run(t, handle, pt.Input{Method: "POST", Body: pt.Body(body)})
	pt.Expect(t, res, "modify", 0)
	got := *res.Body
	for _, leaked := range []string{"jane.doe@example.com", "4111 1111 1111 1111", "123-45-6789", "412 345 678", "192.168.10.20"} {
		if strings.Contains(got, leaked) {
			t.Errorf("%q not redacted: %s", leaked, got)
		}
	}
	// A 16-digit number failing the Luhn check is not a card.
	if !strings.Contains(got, "1234 5678 9012 3456") {
		t.Errorf("non-card number redacted: %s", got)
	}
	if h := res.Headers["X-RelayOps-PII-Redacted"]; h != "email:1,credit_card:1,us_ssn:1,ipv4:1,phone:1" {
		t.Errorf("summary header %q", h)
	}

	clean := pt.Run(t, handle, pt.Input{Method: "POST", Body: pt.Body(`{"prompt":"hello"}`)})
	pt.Expect(t, clean, "allow", 0)

	block := pt.Run(t, handle, pt.Input{Method: "POST", Body: pt.Body("mail me at a@b.io"), Config: map[string]any{"mode": "block"}})
	pt.Expect(t, block, "deny", 422)

	only := pt.Run(t, handle, pt.Input{Method: "POST", Body: pt.Body("a@b.io 123-45-6789"), Config: map[string]any{"detect": []string{"us_ssn"}}})
	if !strings.Contains(*only.Body, "a@b.io") || strings.Contains(*only.Body, "123-45-6789") {
		t.Errorf("detect subset: %s", *only.Body)
	}

	pt.Expect(t, pt.Run(t, handle, pt.Input{Method: "POST"}), "deny", 500)
	pt.Expect(t, pt.Run(t, handle, pt.Input{Method: "POST", Body: pt.Body("x"), Config: map[string]any{"detect": []string{"dna"}}}), "deny", 500)
}
