package config

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

type hostedStub struct {
	raw []byte
	err error
}

func (s hostedStub) QueryRow(context.Context, string, ...any) pgx.Row { return s }
func (s hostedStub) Scan(dest ...any) error {
	if s.err != nil {
		return s.err
	}
	*dest[0].(*[]byte) = s.raw
	return nil
}

func TestHostedEnvironmentRejectsBootstrapOverwriteAtomically(t *testing.T) {
	t.Setenv("RELAYOPS_ADMIN_TOKEN", "existing-token")
	_, err := LoadHostedEnvironment(context.Background(), hostedStub{raw: []byte(`{"secrets":{"RELAYOPS_ADMIN_TOKEN":"replacement","RELAYOPS_DATABASE_URL":"malicious"},"settings":{}}`)})
	if err == nil || os.Getenv("RELAYOPS_ADMIN_TOKEN") != "existing-token" {
		t.Fatal("invalid configuration changed existing environment")
	}
}

func TestHostedEnvironmentSeparatesSecretsAndSettings(t *testing.T) {
	for _, raw := range []string{
		`{"settings":{"RELAYOPS_ADMIN_TOKEN":"secret"}}`,
		`{"secrets":{"SUPABASE_STORAGE_BUCKET":"bucket"}}`,
		`{"secrets":{"RELAYOPS_SECRET_TEST":""}}`,
		`{"secrets":{"RELAYOPS_SECRET_TEST":"bad\u0000value"}}`,
	} {
		if _, err := decodeHostedEnvironment([]byte(raw)); err == nil {
			t.Fatal("accepted invalid placement or value")
		}
	}
}

func TestHostedEnvironmentLoadsSecretReferencesWithoutLeakingErrors(t *testing.T) {
	t.Setenv("RELAYOPS_SECRET_HOSTED_TEST", "")
	t.Setenv("SUPABASE_STORAGE_BUCKET", "")
	n, err := LoadHostedEnvironment(context.Background(), hostedStub{raw: []byte(`{"secrets":{"RELAYOPS_SECRET_HOSTED_TEST":"private-key"},"settings":{"SUPABASE_STORAGE_BUCKET":"private-backups"}}`)})
	if err != nil || n != 2 || ResolveSecrets("Bearer ${secret:HOSTED_TEST}") != "Bearer private-key" {
		t.Fatal("hosted secret did not resolve")
	}
	_, err = LoadHostedEnvironment(context.Background(), hostedStub{err: errors.New("database password=private-key")})
	if err == nil || strings.Contains(err.Error(), "private-key") {
		t.Fatal("database error leaked a secret")
	}
}
