// Package testdb provisions throwaway PostgreSQL databases for integration tests.
//
// Tests run when RELAYOPS_TEST_DATABASE_URL names a server the tests may create
// databases on, e.g. postgres://postgres@localhost:5432/postgres?sslmode=disable.
// Without it they are skipped, unless RELAYOPS_REQUIRE_INTEGRATION=1, in which
// case they fail: CI sets it so a missing database can never pass silently.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Required reports whether integration tests must run.
func Required() bool { return os.Getenv("RELAYOPS_REQUIRE_INTEGRATION") == "1" }

// ServerURL returns the admin DSN, skipping (or failing, when required) without one.
func ServerURL(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv("RELAYOPS_TEST_DATABASE_URL")
	if dsn == "" {
		if Required() {
			t.Fatal("RELAYOPS_REQUIRE_INTEGRATION=1 but RELAYOPS_TEST_DATABASE_URL is not set")
		}
		t.Skip("RELAYOPS_TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	return dsn
}

// New creates an empty database, drops it when the test ends, and returns its DSN.
func New(t testing.TB) string {
	t.Helper()
	dsn := ServerURL(t)
	name := "relayops_it_" + randomSuffix()
	Create(t, dsn, name)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// Create makes database name on the server and drops it at cleanup.
func Create(t testing.TB, serverDSN, name string) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, serverDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		admin.Close()
	})
}

func randomSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Name returns a fresh database name with the integration-test prefix.
func Name() string { return "relayops_it_" + randomSuffix() }
