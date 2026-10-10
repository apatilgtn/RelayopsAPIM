package testingstudio

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/relayops/apim/internal/grpc"
	"github.com/relayops/apim/internal/store"
)

// EvaluateAssertion executes a single assertion against an HTTP response.
func EvaluateAssertion(a store.AssertionDef, respStatusCode int, respHeaders http.Header, bodyBytes []byte, durationMS float64) store.AssertionRes {
	res := store.AssertionRes{
		Type:        a.Type,
		Target:      a.Target,
		Expected:    a.Expected,
		Description: a.Description,
	}

	switch a.Type {
	case "status_code":
		res.Actual = strconv.Itoa(respStatusCode)
		exp := strings.TrimSpace(a.Expected)
		if strings.HasSuffix(exp, "xx") && len(exp) == 3 {
			// Range like 2xx, 4xx, 5xx
			prefix := exp[0]
			res.Passed = len(res.Actual) == 3 && res.Actual[0] == prefix
		} else if strings.Contains(exp, "-") {
			parts := strings.Split(exp, "-")
			low, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
			high, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
			res.Passed = respStatusCode >= low && respStatusCode <= high
		} else {
			expCode, _ := strconv.Atoi(exp)
			res.Passed = respStatusCode == expCode
		}
		if !res.Passed {
			res.Error = fmt.Sprintf("expected status %s, got %d", a.Expected, respStatusCode)
		}

	case "header_equals":
		actual := respHeaders.Get(a.Target)
		res.Actual = actual
		res.Passed = strings.EqualFold(strings.TrimSpace(actual), strings.TrimSpace(a.Expected))
		if !res.Passed {
			res.Error = fmt.Sprintf("header '%s' expected '%s', got '%s'", a.Target, a.Expected, actual)
		}

	case "header_exists":
		_, exists := respHeaders[http.CanonicalHeaderKey(a.Target)]
		if !exists {
			// Also check lower-case
			for k := range respHeaders {
				if strings.EqualFold(k, a.Target) {
					exists = true
					break
				}
			}
		}
		res.Actual = strconv.FormatBool(exists)
		res.Passed = exists
		if !res.Passed {
			res.Error = fmt.Sprintf("required header '%s' was missing from response", a.Target)
		}

	case "body_contains":
		bodyStr := string(bodyBytes)
		res.Passed = strings.Contains(bodyStr, a.Expected)
		if len(bodyStr) > 60 {
			res.Actual = bodyStr[:60] + "…"
		} else {
			res.Actual = bodyStr
		}
		if !res.Passed {
			res.Error = fmt.Sprintf("body did not contain expected substring '%s'", a.Expected)
		}

	case "json_path_equals":
		val, err := extractJSONPointer(bodyBytes, a.Target)
		if err != nil {
			res.Passed = false
			res.Error = fmt.Sprintf("failed to extract JSON pointer '%s': %v", a.Target, err)
			res.Actual = "<error>"
		} else {
			actualStr := fmt.Sprintf("%v", val)
			res.Actual = actualStr
			res.Passed = strings.TrimSpace(actualStr) == strings.TrimSpace(a.Expected)
			if !res.Passed {
				res.Error = fmt.Sprintf("JSON pointer '%s' expected '%s', got '%s'", a.Target, a.Expected, actualStr)
			}
		}

	case "json_path_exists":
		val, err := extractJSONPointer(bodyBytes, a.Target)
		exists := err == nil && val != nil
		res.Actual = strconv.FormatBool(exists)
		res.Passed = exists
		if !res.Passed {
			res.Error = fmt.Sprintf("JSON pointer '%s' does not exist in response body", a.Target)
		}

	case "response_time_ms":
		res.Actual = fmt.Sprintf("%.1f", durationMS)
		maxMS, err := strconv.ParseFloat(strings.TrimSpace(a.Expected), 64)
		if err == nil && maxMS > 0 {
			res.Passed = durationMS <= maxMS
			if !res.Passed {
				res.Error = fmt.Sprintf("response time %.1fms exceeded threshold %sms", durationMS, a.Expected)
			}
		} else {
			res.Passed = true
		}

	case "grpc_status":
		actualStatus := respHeaders.Get("grpc-status")
		if actualStatus == "" {
			actualStatus = strconv.Itoa(respStatusCode)
		}
		res.Actual = actualStatus
		exp := strings.TrimSpace(a.Expected)
		if expCode, err := strconv.Atoi(exp); err == nil {
			actCode, _ := strconv.Atoi(actualStatus)
			res.Passed = actCode == expCode
		} else {
			actCode, _ := strconv.Atoi(actualStatus)
			res.Passed = strings.EqualFold(exp, grpc.StatusText(actCode))
		}
		if !res.Passed {
			res.Error = fmt.Sprintf("gRPC status expected '%s', got '%s'", a.Expected, actualStatus)
		}

	case "graphql_has_no_errors":
		var gqlResp struct {
			Errors []any `json:"errors"`
		}
		if err := json.Unmarshal(bodyBytes, &gqlResp); err != nil {
			res.Passed = false
			res.Error = fmt.Sprintf("failed to parse GraphQL response: %v", err)
			res.Actual = "<invalid json>"
		} else {
			res.Passed = len(gqlResp.Errors) == 0
			res.Actual = fmt.Sprintf("%d errors", len(gqlResp.Errors))
			if !res.Passed {
				res.Error = fmt.Sprintf("GraphQL response contained %d errors", len(gqlResp.Errors))
			}
		}

	case "graphql_data_equals":
		ptr := a.Target
		if !strings.HasPrefix(ptr, "/data") && !strings.HasPrefix(ptr, "/") {
			ptr = "/data/" + ptr
		} else if !strings.HasPrefix(ptr, "/data") && strings.HasPrefix(ptr, "/") {
			ptr = "/data" + ptr
		}
		val, err := extractJSONPointer(bodyBytes, ptr)
		if err != nil {
			res.Passed = false
			res.Error = fmt.Sprintf("failed to extract GraphQL data pointer '%s': %v", ptr, err)
			res.Actual = "<error>"
		} else {
			actualStr := fmt.Sprintf("%v", val)
			res.Actual = actualStr
			res.Passed = strings.TrimSpace(actualStr) == strings.TrimSpace(a.Expected)
			if !res.Passed {
				res.Error = fmt.Sprintf("GraphQL data '%s' expected '%s', got '%s'", ptr, a.Expected, actualStr)
			}
		}

	default:
		res.Passed = false
		res.Error = fmt.Sprintf("unsupported assertion type '%s'", a.Type)
	}

	return res
}

// extractJSONPointer extracts a value from a JSON document using RFC 6901 pointer syntax.
func extractJSONPointer(docBytes []byte, pointer string) (any, error) {
	if len(docBytes) == 0 {
		return nil, fmt.Errorf("empty JSON payload")
	}

	var root any
	if err := json.Unmarshal(docBytes, &root); err != nil {
		return nil, fmt.Errorf("malformed JSON: %w", err)
	}

	pointer = strings.TrimSpace(pointer)
	if pointer == "" || pointer == "/" {
		return root, nil
	}

	if !strings.HasPrefix(pointer, "/") {
		pointer = "/" + pointer
	}

	tokens := strings.Split(pointer[1:], "/")
	current := root

	for _, rawToken := range tokens {
		token := strings.ReplaceAll(strings.ReplaceAll(rawToken, "~1", "/"), "~0", "~")

		switch node := current.(type) {
		case map[string]any:
			val, ok := node[token]
			if !ok {
				return nil, fmt.Errorf("property '%s' not found", token)
			}
			current = val
		case []any:
			idx, err := strconv.Atoi(token)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, fmt.Errorf("array index '%s' out of bounds", token)
			}
			current = node[idx]
		default:
			return nil, fmt.Errorf("cannot traverse into primitive type at '%s'", token)
		}
	}

	return current, nil
}
