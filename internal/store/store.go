// Package store is the Postgres persistence layer for RelayOps.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ConfigChannel is the Postgres NOTIFY channel fired on any config change.
const ConfigChannel = "relayops_config"

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

type Store struct {
	Pool *pgxpool.Pool
}

func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = 20
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{Pool: pool}, nil
}

// OpenLazy creates a store without contacting the database. Connections are made
// on first use, so a process started during a database outage recovers on its
// own when the database returns.
func OpenLazy(url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = 20
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	return &Store{Pool: pool}, nil
}

// IsUnavailable reports whether err means the database could not be reached.
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "failed to connect") || strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "actively refused") || strings.Contains(msg, "connect: ") && strings.Contains(msg, "dial")
}

func (s *Store) Close() { s.Pool.Close() }

// Migrate applies embedded SQL migrations in lexical order, once each.
func (s *Store) Migrate(ctx context.Context) error {
	if err := s.MigrateTo(ctx, ""); err != nil {
		return err
	}
	s.EnsureBaselineRevisionSnapshot(ctx)
	return nil
}

// MigrateTo applies migrations up to and including version (all when empty).
// Upgrade tests use it to build the schema of an earlier release.
func (s *Store) MigrateTo(ctx context.Context, upTo string) error {
	if _, err := s.Pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	files, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		version := strings.TrimSuffix(strings.TrimPrefix(f, "migrations/"), ".sql")
		if versionLimit(version, upTo) {
			break
		}
		var exists bool
		if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, version).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sqlBytes, err := migrationFS.ReadFile(f)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
				return fmt.Errorf("migration %s: %w", version, err)
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1)`, version)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// mapErr converts driver errors into store sentinel errors.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%w: %s", ErrConflict, pgErr.Detail)
		case "23503": // foreign_key_violation
			return fmt.Errorf("%w: %s", ErrConflict, pgErr.Detail)
		case "22P02", "23514": // invalid uuid text / check violation
			return fmt.Errorf("invalid input: %s", pgErr.Message)
		}
	}
	return err
}

func IsInvalidInput(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "invalid input")
}

// versionLimit reports whether version is past the upTo limit.
func versionLimit(version, upTo string) bool { return upTo != "" && version > upTo }
