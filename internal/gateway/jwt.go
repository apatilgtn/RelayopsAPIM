package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var (
	errTokenMalformed = errors.New("malformed token")
	errTokenAlg       = errors.New("unsupported token algorithm (HS256 required)")
	errTokenSignature = errors.New("invalid token signature")
	errTokenExpired   = errors.New("token expired")
	errTokenNotYet    = errors.New("token not yet valid")
	errTokenNoExp     = errors.New("token missing required exp claim")
	errSecretEmpty    = errors.New("jwt secret not configured on api")
)

// verifyHS256 validates a compact JWS signed with HMAC-SHA256 and strictly enforces claims.
func verifyHS256(token, secret string, now time.Time) (map[string]any, error) {
	if secret == "" {
		return nil, errSecretEmpty
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errTokenMalformed
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errTokenMalformed
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, errTokenMalformed
	}
	if header.Alg != "HS256" {
		return nil, errTokenAlg
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errTokenMalformed
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, errTokenSignature
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errTokenMalformed
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, errTokenMalformed
	}
	exp, hasExp := claims["exp"].(float64)
	if !hasExp {
		return nil, errTokenNoExp
	}
	const leeway = 30 * time.Second
	if now.After(time.Unix(int64(exp), 0).Add(leeway)) {
		return nil, errTokenExpired
	}
	if nbf, ok := claims["nbf"].(float64); ok && now.Add(leeway).Before(time.Unix(int64(nbf), 0)) {
		return nil, errTokenNotYet
	}
	return claims, nil
}
