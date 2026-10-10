package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"

	"github.com/jackc/pgx/v5"
)

type HostedReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

var hostedSecretName = regexp.MustCompile(`^RELAYOPS_SECRET_[A-Z][A-Z0-9_]*$`)

// HostedSecret names are deliberately restricted: Vault cannot overwrite the
// database bootstrap URL, node identity, listener addresses or enable flags.
func HostedSecret(name string) bool {
	return name == "RELAYOPS_ADMIN_TOKEN" || name == "SUPABASE_S3_ACCESS_KEY_ID" ||
		name == "SUPABASE_S3_SECRET_ACCESS_KEY" || hostedSecretName.MatchString(name)
}

func HostedSetting(name string) bool {
	switch name {
	case "RELAYOPS_PUBLIC_URL", "RELAYOPS_LOG_RETENTION_HOURS", "RELAYOPS_LOG_SPOOL_MB",
		"SUPABASE_S3_ENDPOINT", "SUPABASE_S3_REGION", "SUPABASE_STORAGE_BUCKET", "SUPABASE_DOCUMENTS_BUCKET":
		return true
	}
	return false
}

// LoadHostedEnvironment reads production values over the backend SQL connection.
// No decrypted values are returned by an HTTP endpoint or written to a cache.
// Call before starting goroutines; values remain in process memory until restart.
func LoadHostedEnvironment(ctx context.Context, reader HostedReader) (int, error) {
	var raw []byte
	err := reader.QueryRow(ctx, `
		SELECT jsonb_build_object(
		  'settings', coalesce((SELECT jsonb_object_agg(name,value)
		    FROM relayops_private.runtime_settings), '{}'::jsonb),
		  'secrets', coalesce((SELECT jsonb_object_agg(substring(name FROM 14),decrypted_secret)
		    FROM vault.decrypted_secrets WHERE name LIKE 'relayops.env.%'), '{}'::jsonb))
	`).Scan(&raw)
	if err != nil {
		// Database diagnostics may contain credentials; never propagate them here.
		return 0, errors.New("hosted configuration could not be read")
	}
	values, err := decodeHostedEnvironment(raw)
	if err != nil {
		return 0, err
	}
	for name, value := range values {
		if err := os.Setenv(name, value); err != nil {
			return 0, errors.New("hosted configuration could not be installed")
		}
	}
	return len(values), nil
}

func decodeHostedEnvironment(raw []byte) (map[string]string, error) {
	var doc struct {
		Settings map[string]string `json:"settings"`
		Secrets  map[string]string `json:"secrets"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil, errors.New("hosted configuration is invalid")
	}
	values := make(map[string]string)
	for name, value := range doc.Settings {
		if !HostedSetting(name) || !validHostedValue(value) {
			return nil, errors.New("hosted configuration contains an invalid setting")
		}
		values[name] = value
	}
	for name, value := range doc.Secrets {
		if !HostedSecret(name) || value == "" || !validHostedValue(value) {
			return nil, errors.New("hosted configuration contains an invalid secret")
		}
		values[name] = value
	}
	return values, nil
}

func validHostedValue(value string) bool {
	for _, ch := range value {
		if ch == 0 {
			return false
		}
	}
	return true
}
