// relayops-secrets stores namespaced production secrets without printing values.
// The store command accepts JSON on stdin; run loads Vault values into a child
// process, e.g. the Storage backup script. It never offers a plaintext export.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/relayops/apim/internal/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "secret operation failed:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: relayops-secrets store | run -- COMMAND [ARG...]")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, config.Load().DatabaseURL)
	if err != nil {
		return errors.New("database configuration is invalid")
	}
	defer pool.Close()
	switch os.Args[1] {
	case "store":
		var doc struct {
			Secrets  map[string]string `json:"secrets"`
			Settings map[string]string `json:"settings"`
		}
		dec := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20))
		dec.DisallowUnknownFields()
		if dec.Decode(&doc) != nil {
			return errors.New("invalid input document")
		}
		for name, value := range doc.Secrets {
			if !config.HostedSecret(name) || value == "" {
				return errors.New("invalid secret name or value")
			}
		}
		for name := range doc.Settings {
			if !config.HostedSetting(name) {
				return errors.New("invalid setting name")
			}
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return errors.New("cannot begin secret transaction")
		}
		defer tx.Rollback(context.Background())
		// Serializes stores so two operators cannot create duplicate named secrets.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(282583951406)"); err != nil {
			return errors.New("cannot lock secret transaction")
		}
		for name, value := range doc.Secrets {
			var id string
			err := tx.QueryRow(ctx, "SELECT id::text FROM vault.secrets WHERE name=$1", "relayops.env."+name).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				_, err = tx.Exec(ctx, "SELECT vault.create_secret($1,$2,$3)", value, "relayops.env."+name, "RelayOps production server secret")
			} else if err == nil {
				_, err = tx.Exec(ctx, "SELECT vault.update_secret($1::uuid,$2)", id, value)
			}
			if err != nil {
				return errors.New("cannot store Vault secret")
			}
		}
		for name, value := range doc.Settings {
			if _, err := tx.Exec(ctx, "INSERT INTO relayops_private.runtime_settings(name,value) VALUES($1,$2) ON CONFLICT(name) DO UPDATE SET value=excluded.value,updated_at=now()", name, value); err != nil {
				return errors.New("cannot store runtime setting")
			}
		}
		if tx.Commit(ctx) != nil {
			return errors.New("cannot commit secret transaction")
		}
		fmt.Printf("Stored %d secrets and %d private settings\n", len(doc.Secrets), len(doc.Settings))
		return nil
	case "run":
		if len(os.Args) < 4 || os.Args[2] != "--" {
			return errors.New("usage: relayops-secrets run -- COMMAND [ARG...]")
		}
		if _, err := config.LoadHostedEnvironment(ctx, pool); err != nil {
			return err
		}
		child := exec.Command(os.Args[3], os.Args[4:]...)
		child.Env = os.Environ()
		child.Stdout, child.Stderr, child.Stdin = os.Stdout, os.Stderr, os.Stdin
		if child.Run() != nil {
			return errors.New("child command failed")
		}
		return nil
	default:
		return errors.New("unknown secret operation")
	}
}
