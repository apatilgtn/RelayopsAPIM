package testingstudio

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/relayops/apim/internal/store"
)

var (
	varNameRegex = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,63}$`)
	validMethods = map[string]bool{
		"GET": true, "POST": true, "PUT": true, "PATCH": true,
		"DELETE": true, "HEAD": true, "OPTIONS": true,
	}
	validAssertionTypes = map[string]bool{
		"status_code": true, "header_equals": true, "header_exists": true,
		"json_path_equals": true, "json_path_exists": true,
		"body_contains": true, "response_time_ms": true,
		"grpc_status": true, "graphql_data_equals": true, "graphql_has_no_errors": true,
	}
)

const (
	MaxRequestsPerSuite = 100
	MaxRequestBodyBytes = 1 << 20  // 1 MiB
	MaxResponseBodyCap  = 5 << 20  // 5 MiB
	MaxRedactedPreview  = 16 << 10 // 16 KiB
)

// ValidateSuiteDefinition validates that a test suite definition conforms to structural and safety constraints.
func ValidateSuiteDefinition(def store.SuiteDefinition) error {
	if strings.TrimSpace(def.Name) == "" {
		return errors.New("suite name is required")
	}
	if len(def.Requests) == 0 {
		return errors.New("suite must contain at least one request definition")
	}
	if len(def.Requests) > MaxRequestsPerSuite {
		return fmt.Errorf("suite exceeds maximum allowed requests limit (%d > %d)", len(def.Requests), MaxRequestsPerSuite)
	}

	for i, v := range def.Variables {
		if !varNameRegex.MatchString(v.Key) {
			return fmt.Errorf("variable[%d] key '%s' is invalid (must match %s)", i, v.Key, varNameRegex.String())
		}
	}

	if len(def.Comparison.Headers) > 32 {
		return errors.New("comparison supports at most 32 selected headers")
	}
	for _, h := range def.Comparison.Headers {
		key := strings.ToLower(strings.TrimSpace(h))
		if !regexp.MustCompile(`^[a-z0-9!#$%&'*+.^_`+"`"+`|~-]+$`).MatchString(key) || sensitiveHeaders[key] || strings.HasPrefix(key, "x-relayops-") {
			return fmt.Errorf("comparison header %q is invalid, sensitive or reserved", h)
		}
	}
	for _, v := range []float64{def.Comparison.MaxLatencyIncreasePercent, def.Comparison.MinLatencyIncreaseMS} {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("comparison latency thresholds must be finite and non-negative")
		}
	}
	if def.Comparison.MinLatencyIncreaseMS > 0 && def.Comparison.MaxLatencyIncreasePercent == 0 {
		return errors.New("minimum latency increase requires a percentage threshold")
	}
	for _, p := range def.IgnorePaths {
		if p == "" || !strings.HasPrefix(p, "/") || strings.TrimSpace(p) != p {
			return errors.New("ignored body paths must be exact JSON pointers starting with /")
		}
		for i := 0; i < len(p); i++ {
			if p[i] == '~' {
				if i+1 >= len(p) || (p[i+1] != '0' && p[i+1] != '1') {
					return errors.New("invalid JSON pointer escape in ignored path")
				}
				i++
			}
		}
	}

	seenIDs := make(map[string]bool)
	for i, req := range def.Requests {
		if strings.TrimSpace(req.Name) == "" {
			return fmt.Errorf("request[%d] name is required", i)
		}
		if req.ID != "" {
			if seenIDs[req.ID] {
				return fmt.Errorf("duplicate request ID '%s'", req.ID)
			}
			seenIDs[req.ID] = true
		}

		method := strings.ToUpper(strings.TrimSpace(req.Method))
		if !validMethods[method] {
			return fmt.Errorf("request[%d] ('%s') has unsupported HTTP method '%s'", i, req.Name, req.Method)
		}

		if err := ValidateRequestPath(req.Path); err != nil {
			return fmt.Errorf("request[%d] ('%s') has invalid path: %w", i, req.Name, err)
		}

		if len(req.Body) > MaxRequestBodyBytes {
			return fmt.Errorf("request[%d] ('%s') body exceeds 1 MiB limit (%d bytes)", i, req.Name, len(req.Body))
		}

		for j, a := range req.Assertions {
			if !validAssertionTypes[a.Type] {
				return fmt.Errorf("request[%d] assertion[%d] has unsupported type '%s'", i, j, a.Type)
			}
			if strings.TrimSpace(a.Expected) == "" && a.Type != "header_exists" && a.Type != "json_path_exists" && a.Type != "graphql_has_no_errors" {
				return fmt.Errorf("request[%d] assertion[%d] ('%s') expected value cannot be empty", i, j, a.Type)
			}
		}

		for j, ext := range req.Extracts {
			if !varNameRegex.MatchString(ext.VarName) {
				return fmt.Errorf("request[%d] extract[%d] variable name '%s' is invalid", i, j, ext.VarName)
			}
			if ext.Source != "json_path" && ext.Source != "header" {
				return fmt.Errorf("request[%d] extract[%d] unsupported source '%s'", i, j, ext.Source)
			}
			if strings.TrimSpace(ext.Target) == "" {
				return fmt.Errorf("request[%d] extract[%d] target path or header name is required", i, j)
			}
		}
	}

	return nil
}

// ValidateRequestPath checks for traversal, absolute URI injection, and control characters.
func ValidateRequestPath(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("path cannot be empty")
	}

	if strings.Contains(path, "://") {
		return errors.New("absolute URLs are forbidden; use relative endpoint path through approved gateway target")
	}

	if strings.Contains(path, "..") {
		return errors.New("directory traversal ('..') is strictly prohibited")
	}

	for _, ch := range path {
		if ch < 32 || ch == 127 {
			return errors.New("path contains illegal control characters")
		}
	}

	return nil
}

// ValidateGatewayTarget ensures target belongs to permitted gateway destinations (Anti-SSRF).
func ValidateGatewayTarget(target string) (*url.URL, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, errors.New("gateway target cannot be empty")
	}

	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("invalid gateway target URL: %w", err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme '%s' (only http/https permitted)", u.Scheme)
	}

	hostname := strings.ToLower(u.Hostname())
	if hostname == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("gateway target must have a host and no credentials or fragment")
	}

	// Block AWS/GCP/Azure/DigitalOcean cloud metadata hostnames and link-local IPv4
	if hostname == "169.254.169.254" || strings.HasPrefix(hostname, "169.254.") ||
		hostname == "metadata.google.internal" || hostname == "metadata" ||
		hostname == "instance-data" {
		return nil, errors.New("access to link-local metadata address or cloud metadata hostname is strictly blocked")
	}

	// Block IPv6 link-local addresses
	if strings.HasPrefix(hostname, "fe80:") || strings.HasPrefix(hostname, "[fe80:") {
		return nil, errors.New("access to link-local IPv6 addresses is strictly blocked")
	}

	// Restrict dangerous internal infrastructure ports
	portStr := u.Port()
	if portStr != "" {
		port, pErr := strconv.Atoi(portStr)
		if pErr == nil {
			restrictedPorts := map[int]string{
				22:    "SSH",
				23:    "Telnet",
				25:    "SMTP",
				53:    "DNS",
				2379:  "etcd",
				2380:  "etcd",
				3306:  "MySQL",
				5432:  "PostgreSQL",
				6379:  "Redis",
				9200:  "Elasticsearch",
				11211: "Memcached",
				27017: "MongoDB",
			}
			if service, restricted := restrictedPorts[port]; restricted {
				return nil, fmt.Errorf("access to internal infrastructure port %d (%s) is strictly forbidden", port, service)
			}
		}
	}

	// Connection-time DNS validation is enforced by safeTransport.

	return u, nil
}

// IsTrustedGateway checks if a target URL points to a trusted local or cluster gateway node.
func IsTrustedGateway(targetURL string, extraApproved ...string) bool {
	u, err := url.Parse(targetURL)
	if err != nil {
		return false
	}
	if u.User != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	approved := append(append([]string{}, extraApproved...), strings.Split(os.Getenv("RELAYOPS_APPROVED_GATEWAYS"), ",")...)
	for _, app := range approved {
		a, err := url.Parse(strings.TrimSpace(app))
		if err == nil && a.User == nil && a.Hostname() != "" && strings.EqualFold(a.Scheme, u.Scheme) && strings.EqualFold(a.Hostname(), u.Hostname()) && originPort(a) == originPort(u) {
			return true
		}
	}
	return false
}

func originPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Port()
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}
