// Command pii-redactor removes personal data from request bodies before they
// reach the upstream, for example prompts sent to a model provider.
//
//	"wasm_body_limit_bytes": 262144,
//	"wasm_response_body_limit_bytes": 1048576,
//	"wasm_config": {"pii-redactor": {
//	  "detect": ["email", "credit_card", "us_ssn", "phone", "ipv4"],
//	  "mode":   "redact",
//	  "scan":   ["request", "response"]
//	}}
//
// scan picks the directions (default: request). Scanning responses keeps
// personal data in upstream answers (or MCP tool results) from reaching the
// caller. A response body the gateway could not hand over (streamed or
// over the limit) is marked X-RelayOps-PII-Unscanned, or refused in block
// mode.
//
// mode "redact" (default) replaces each match with [REDACTED:<kind>] and
// reports counts in X-RelayOps-PII-Redacted; mode "block" refuses the request
// with 422 instead. Card numbers are only matched when they pass the Luhn
// check. Detection is pattern-based: it reduces exposure, it does not prove
// the absence of personal data.
package main

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	plugin "github.com/relayops/apim/sdk/wasmplugin"
)

var detectors = map[string]*regexp.Regexp{
	"email":       regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`),
	"credit_card": regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`),
	"us_ssn":      regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),
	"phone":       regexp.MustCompile(`(?:\+\d{1,3}[ .-]?)?\(?\d{2,4}\)?[ .-]\d{3,4}[ .-]\d{3,4}\b`),
	"ipv4":        regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b`),
}

// order applies detectors so longer, more specific patterns win.
var order = []string{"email", "credit_card", "us_ssn", "ipv4", "phone"}

type config struct {
	Detect []string `json:"detect"`
	Mode   string   `json:"mode"`
	Scan   []string `json:"scan"`
}

func build(c *config) error {
	if len(c.Detect) == 0 {
		c.Detect = order
	}
	for _, d := range c.Detect {
		if detectors[d] == nil {
			return fmt.Errorf("unknown detector %q (use %s)", d, strings.Join(order, ", "))
		}
	}
	switch c.Mode {
	case "":
		c.Mode = "redact"
	case "redact", "block":
	default:
		return fmt.Errorf("mode must be redact or block")
	}
	if len(c.Scan) == 0 {
		c.Scan = []string{"request"}
	}
	for _, d := range c.Scan {
		if d != "request" && d != "response" {
			return fmt.Errorf("scan entries must be request or response")
		}
	}
	return nil
}

func init() {
	plugin.Handle(handle)
	plugin.HandleResponse(handleResponse)
}

func main() {}

func handle(r plugin.Request) plugin.Result {
	cfg, err := plugin.Config(r, build)
	if err != nil {
		return plugin.Misconfigured(err)
	}
	if !slices.Contains(cfg.Scan, "request") {
		return plugin.Allow()
	}
	if r.Body == nil {
		return plugin.Misconfigured(fmt.Errorf("pii-redactor needs traffic_policy.wasm_body_limit_bytes"))
	}
	body, counts := redact(*r.Body, cfg.Detect)
	if len(counts) == 0 {
		return plugin.Allow()
	}
	if cfg.Mode == "block" {
		return plugin.Deny(http.StatusUnprocessableEntity, "pii_detected", "request contains personal data ("+summarize(counts, ", ")+")")
	}
	return plugin.Modify().SetBody(body).SetHeader("X-RelayOps-PII-Redacted", summarize(counts, ","))
}

func handleResponse(r plugin.Response) plugin.Result {
	cfg, err := plugin.ResponseConfig(r, build)
	if err != nil {
		return plugin.Deny(http.StatusBadGateway, "plugin_misconfigured", err.Error())
	}
	if !slices.Contains(cfg.Scan, "response") {
		return plugin.Allow()
	}
	if r.Body == nil {
		if cfg.Mode == "block" {
			return plugin.Deny(http.StatusBadGateway, "pii_unscanned", "the response could not be scanned for personal data")
		}
		return plugin.Modify().SetHeader("X-RelayOps-PII-Unscanned", "response body not available to the gateway")
	}
	body, counts := redact(*r.Body, cfg.Detect)
	if len(counts) == 0 {
		return plugin.Allow()
	}
	if cfg.Mode == "block" {
		return plugin.Deny(http.StatusBadGateway, "pii_in_response", "the response contains personal data ("+summarize(counts, ", ")+")")
	}
	return plugin.Modify().SetBody(body).SetHeader("X-RelayOps-PII-Redacted", summarize(counts, ","))
}

func summarize(counts map[string]int, sep string) string {
	out := make([]string, 0, len(counts))
	for _, kind := range order {
		if n := counts[kind]; n > 0 {
			out = append(out, fmt.Sprintf("%s:%d", kind, n))
		}
	}
	return strings.Join(out, sep)
}

func redact(body string, detect []string) (string, map[string]int) {
	counts := map[string]int{}
	for _, kind := range order {
		if !slices.Contains(detect, kind) {
			continue
		}
		var b strings.Builder
		last := 0
		for _, loc := range detectors[kind].FindAllStringIndex(body, -1) {
			m := body[loc[0]:loc[1]]
			if kind == "credit_card" && !luhn(m) {
				continue
			}
			if kind == "phone" && partOfLongerNumber(body, loc[0], loc[1]) {
				continue
			}
			b.WriteString(body[last:loc[0]])
			b.WriteString("[REDACTED:" + kind + "]")
			last = loc[1]
			counts[kind]++
		}
		b.WriteString(body[last:])
		body = b.String()
	}
	return body, counts
}

// partOfLongerNumber reports whether a match continues into more digits
// (separated by spaces, dots or dashes), as in an order or account number.
func partOfLongerNumber(s string, start, end int) bool {
	isDigit := func(i int) bool { return i >= 0 && i < len(s) && s[i] >= '0' && s[i] <= '9' }
	isSep := func(i int) bool { return i >= 0 && i < len(s) && (s[i] == ' ' || s[i] == '-' || s[i] == '.') }
	before := start - 1
	if isSep(before) {
		before--
	}
	after := end
	if isSep(after) {
		after++
	}
	return isDigit(before) || isDigit(after)
}

// luhn validates a card number, ignoring spaces and dashes.
func luhn(s string) bool {
	sum, n := 0, 0
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		if c == ' ' || c == '-' {
			continue
		}
		d := int(c - '0')
		if n%2 == 1 {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		n++
	}
	return n >= 13 && sum%10 == 0
}
