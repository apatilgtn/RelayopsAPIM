// Command hmac-auth authenticates callers that sign requests with a shared
// secret, a common scheme for webhooks and partner integrations.
//
//	"wasm_body_limit_bytes": 65536,
//	"wasm_config": {"hmac-auth": {
//	  "keys": {"partner-a": "a-long-random-secret"},
//	  "max_skew_seconds": 300
//	}}
//
// The caller sends:
//
//	X-Key-Id:    partner-a
//	X-Timestamp: <unix seconds>
//	X-Signature: hex(HMAC-SHA256(secret, METHOD "\n" PATH "\n" TIMESTAMP "\n" hex(SHA-256(body))))
//
// On success the signature headers are removed and X-Authenticated-Key-Id is
// set for the upstream. Secrets in wasm_config are part of the API's
// configuration (like a JWT secret): restrict who can edit the API.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"

	plugin "github.com/relayops/apim/sdk/wasmplugin"
)

type config struct {
	Keys           map[string]string `json:"keys"`
	MaxSkewSeconds int64             `json:"max_skew_seconds"`
}

func build(c *config) error {
	if len(c.Keys) == 0 {
		return fmt.Errorf("hmac-auth needs at least one key")
	}
	for id, secret := range c.Keys {
		if len(secret) < 16 {
			return fmt.Errorf("secret for key %q must be at least 16 characters", id)
		}
	}
	if c.MaxSkewSeconds <= 0 {
		c.MaxSkewSeconds = 300
	}
	return nil
}

func init() { plugin.Handle(handle) }

func main() {}

// Sign computes the signature a caller must send.
func Sign(secret, method, path, timestamp, body string) string {
	bodySum := sha256.Sum256([]byte(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(method + "\n" + path + "\n" + timestamp + "\n" + hex.EncodeToString(bodySum[:])))
	return hex.EncodeToString(mac.Sum(nil))
}

func handle(r plugin.Request) plugin.Result {
	cfg, err := plugin.Config(r, build)
	if err != nil {
		return plugin.Misconfigured(err)
	}
	if r.Body == nil {
		return plugin.Misconfigured(fmt.Errorf("hmac-auth needs traffic_policy.wasm_body_limit_bytes to verify the body"))
	}
	keyID, ts, sig := r.Header("X-Key-Id"), r.Header("X-Timestamp"), r.Header("X-Signature")
	if keyID == "" || ts == "" || sig == "" {
		return plugin.Deny(http.StatusUnauthorized, "signature_required", "send X-Key-Id, X-Timestamp and X-Signature")
	}
	secret, ok := cfg.Keys[keyID]
	if !ok {
		return plugin.Deny(http.StatusUnauthorized, "invalid_signature", "signature does not match")
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return plugin.Deny(http.StatusUnauthorized, "invalid_timestamp", "X-Timestamp must be Unix seconds")
	}
	if skew := plugin.NowMS()/1000 - sec; skew > cfg.MaxSkewSeconds || skew < -cfg.MaxSkewSeconds {
		return plugin.Deny(http.StatusUnauthorized, "signature_expired", "X-Timestamp is outside the allowed clock skew")
	}
	want := Sign(secret, r.Method, r.Path, ts, *r.Body)
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return plugin.Deny(http.StatusUnauthorized, "invalid_signature", "signature does not match")
	}
	return plugin.Modify().
		RemoveHeader("X-Signature").
		RemoveHeader("X-Timestamp").
		RemoveHeader("X-Key-Id").
		SetHeader("X-Authenticated-Key-Id", keyID)
}
