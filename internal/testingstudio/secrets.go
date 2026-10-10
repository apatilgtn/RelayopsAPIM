package testingstudio

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

var (
	sensitiveHeaders = map[string]bool{
		"authorization":       true,
		"x-api-key":           true,
		"cookie":              true,
		"set-cookie":          true,
		"proxy-authorization": true,
	}

	secretPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(api[_-]?key|secret|token|password|bearer)["']?\s*[:=]\s*["']?([^"',\s]{4,})`),
		regexp.MustCompile(`(sk-[a-zA-Z0-9]{20,})`),
		regexp.MustCompile(`(rk_[a-zA-Z0-9]{20,})`),
	}
)

// MaskSecret returns a masked representation of a secret string.
func MaskSecret(secret string) string {
	s := strings.TrimSpace(secret)
	if len(s) <= 6 {
		return "••••••••"
	}
	return s[:3] + "••••••••" + s[len(s)-2:]
}

// RedactHeaders returns a sanitized copy of HTTP headers with sensitive tokens masked.
func RedactHeaders(headers http.Header) map[string]string {
	out := make(map[string]string)
	for k, vals := range headers {
		lower := strings.ToLower(k)
		val := strings.Join(vals, ", ")
		if sensitiveHeaders[lower] {
			out[k] = MaskSecret(val)
		} else {
			out[k] = val
		}
	}
	return out
}

// RedactMapHeaders redacts a string map of headers.
func RedactMapHeaders(headers map[string]string) map[string]string {
	out := make(map[string]string)
	for k, val := range headers {
		lower := strings.ToLower(k)
		if sensitiveHeaders[lower] {
			out[k] = MaskSecret(val)
		} else {
			out[k] = val
		}
	}
	return out
}

// RedactBody returns a capped and sanitized preview of a request or response body.
func RedactBody(bodyBytes []byte, maxBytes int) string {
	if len(bodyBytes) == 0 {
		return ""
	}

	bodyStr := string(bodyBytes)
	if len(bodyStr) > maxBytes {
		bodyStr = bodyStr[:maxBytes] + "… [truncated]"
	}

	for _, pattern := range secretPatterns {
		bodyStr = pattern.ReplaceAllString(bodyStr, "$1: [REDACTED]")
	}

	return bodyStr
}

// SanitizePayloadMap sanitizes a payload map for storage.
func SanitizePayloadMap(headers map[string]string, bodyBytes []byte) map[string]any {
	m := map[string]any{
		"headers": RedactMapHeaders(headers),
	}
	if len(bodyBytes) > 0 {
		preview := RedactBody(bodyBytes, MaxRedactedPreview)
		var obj any
		if json.Unmarshal([]byte(preview), &obj) == nil {
			m["body"] = obj
		} else {
			m["body"] = preview
		}
	}
	return m
}
