package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrCanaryInFlight is returned when a fleet-wide publish is attempted while a
// canary revision is being evaluated. Publishing would snapshot the canary's
// table changes into an active revision and silently promote them.
var ErrCanaryInFlight = fmt.Errorf("%w: a canary revision is in flight; promote or abort it before publishing", ErrConflict)

// AutoRollbackSettings is the durable, cluster-wide auto-rollback configuration
// and the state of its last decision.
type AutoRollbackSettings struct {
	Enabled                   bool    `json:"enabled"`
	ErrorRateThresholdPercent float64 `json:"error_rate_threshold_percent"`
	EvaluationWindowSeconds   int     `json:"evaluation_window_seconds"`
	MinRequests               int     `json:"min_requests"`
	CooldownSeconds           int     `json:"cooldown_seconds"`
	// MinBaselineRequests is the baseline sample needed for a relative comparison.
	MinBaselineRequests int `json:"min_baseline_requests"`
	// InsufficientBaselineAction: "absolute" acts on the threshold alone when the
	// baseline is too small; "hold" never acts without comparative evidence.
	InsufficientBaselineAction string     `json:"insufficient_baseline_action"`
	CooldownUntil              *time.Time `json:"cooldown_until,omitempty"`
	LastTriggeredRevision      int64      `json:"last_triggered_revision,omitempty"`
	LastTriggeredAt            *time.Time `json:"last_triggered_at,omitempty"`
	LastTriggeredReason        string     `json:"last_triggered_reason,omitempty"`
	LastEvaluatedAt            *time.Time `json:"last_evaluated_at,omitempty"`
	LastEvaluatedBy            string     `json:"last_evaluated_by,omitempty"`
	UpdatedAt                  time.Time  `json:"updated_at"`
	UpdatedBy                  string     `json:"updated_by,omitempty"`
}

const autoRollbackCols = `enabled, error_rate_threshold_percent, evaluation_window_seconds, min_requests, cooldown_seconds,
	cooldown_until, last_triggered_revision, last_triggered_at, last_triggered_reason, last_evaluated_at, last_evaluated_by,
	updated_at, updated_by, min_baseline_requests, insufficient_baseline_action`

func scanAutoRollback(row pgx.Row) (AutoRollbackSettings, error) {
	var a AutoRollbackSettings
	err := row.Scan(&a.Enabled, &a.ErrorRateThresholdPercent, &a.EvaluationWindowSeconds, &a.MinRequests, &a.CooldownSeconds,
		&a.CooldownUntil, &a.LastTriggeredRevision, &a.LastTriggeredAt, &a.LastTriggeredReason, &a.LastEvaluatedAt,
		&a.LastEvaluatedBy, &a.UpdatedAt, &a.UpdatedBy, &a.MinBaselineRequests, &a.InsufficientBaselineAction)
	return a, mapErr(err)
}

func (s *Store) GetAutoRollbackSettings(ctx context.Context) (AutoRollbackSettings, error) {
	// Read first: the supervisor calls this every few seconds, and the row
	// (seeded by migration 006) almost always exists.
	st, err := scanAutoRollback(s.Pool.QueryRow(ctx, `SELECT `+autoRollbackCols+` FROM auto_rollback_settings WHERE id=1`))
	if !errors.Is(err, ErrNotFound) {
		return st, err
	}
	_, _ = s.Pool.Exec(ctx, `INSERT INTO auto_rollback_settings (id) VALUES (1) ON CONFLICT (id) DO NOTHING`)
	return scanAutoRollback(s.Pool.QueryRow(ctx, `SELECT `+autoRollbackCols+` FROM auto_rollback_settings WHERE id=1`))
}

// UpdateAutoRollbackSettings persists the tunable fields. resetCooldown clears an active cooldown.
func (s *Store) UpdateAutoRollbackSettings(ctx context.Context, in AutoRollbackSettings, resetCooldown bool, actor string) (AutoRollbackSettings, error) {
	return scanAutoRollback(s.Pool.QueryRow(ctx, `INSERT INTO auto_rollback_settings
		(id, enabled, error_rate_threshold_percent, evaluation_window_seconds, min_requests, cooldown_seconds, updated_at, updated_by,
		 min_baseline_requests, insufficient_baseline_action)
		VALUES (1, $1, $2, $3, $4, $5, now(), $6, $8, $9)
		ON CONFLICT (id) DO UPDATE SET enabled=$1, error_rate_threshold_percent=$2, evaluation_window_seconds=$3,
			min_requests=$4, cooldown_seconds=$5, updated_at=now(), updated_by=$6,
			min_baseline_requests=$8, insufficient_baseline_action=$9,
			cooldown_until = CASE WHEN $7 THEN NULL ELSE auto_rollback_settings.cooldown_until END
		RETURNING `+autoRollbackCols,
		in.Enabled, in.ErrorRateThresholdPercent, in.EvaluationWindowSeconds, in.MinRequests, in.CooldownSeconds, actor, resetCooldown,
		in.MinBaselineRequests, in.InsufficientBaselineAction))
}

func (s *Store) MarkAutoRollbackEvaluated(ctx context.Context, by string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE auto_rollback_settings SET last_evaluated_at=now(), last_evaluated_by=$1 WHERE id=1`, by)
	return err
}

func (s *Store) RecordAutoRollbackTrigger(ctx context.Context, revision int64, reason string, cooldown time.Duration) error {
	_, err := s.Pool.Exec(ctx, `UPDATE auto_rollback_settings SET last_triggered_revision=$1, last_triggered_at=now(),
		last_triggered_reason=$2, cooldown_until=now() + make_interval(secs => $3) WHERE id=1`,
		revision, reason, cooldown.Seconds())
	return err
}

// WithAdvisoryLock runs fn only if this process wins the transaction-scoped
// Postgres advisory lock identified by key. The lock is released when fn returns
// or if the connection dies, so a crashed control-plane node never wedges it.
func (s *Store) WithAdvisoryLock(ctx context.Context, key int64, fn func(ctx context.Context) error) (bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var got bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, key).Scan(&got); err != nil {
		return false, err
	}
	if !got {
		return false, nil
	}
	if err := fn(ctx); err != nil {
		return true, err
	}
	return true, tx.Commit(ctx)
}

// RevisionStats is the fleet-wide request outcome for one revision in a window.
type RevisionStats struct {
	Revision int64 `json:"revision"`
	Requests int64 `json:"requests"`
	Errors   int64 `json:"errors"` // HTTP 5xx, including gateway-generated 502/503/504
}

// RevisionErrorStats aggregates request logs from every gateway node over the
// last window.
func (s *Store) RevisionErrorStats(ctx context.Context, window time.Duration, revisions ...int64) (map[int64]RevisionStats, error) {
	now := time.Now()
	return s.RevisionErrorStatsBetween(ctx, now.Add(-window), now.Add(time.Minute), revisions...)
}

// RevisionErrorStatsBetween aggregates request logs from every gateway node in [from, to).
func (s *Store) RevisionErrorStatsBetween(ctx context.Context, from, to time.Time, revisions ...int64) (map[int64]RevisionStats, error) {
	out := make(map[int64]RevisionStats, len(revisions))
	for _, r := range revisions {
		out[r] = RevisionStats{Revision: r}
	}
	if len(revisions) == 0 {
		return out, nil
	}
	// Weighted: with request-log sampling a kept success stands for several
	// requests; errors are always kept with weight 1.
	rows, err := s.Pool.Query(ctx, `SELECT config_revision, round(sum(sample_weight))::bigint,
		round(COALESCE(sum(sample_weight) FILTER (WHERE status >= 500), 0))::bigint
		FROM request_logs
		WHERE ts >= $1 AND ts < $3 AND config_revision = ANY($2)
		GROUP BY config_revision`, from, revisions, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var st RevisionStats
		if err := rows.Scan(&st.Revision, &st.Requests, &st.Errors); err != nil {
			return nil, err
		}
		out[st.Revision] = st
	}
	return out, rows.Err()
}

// PreviousActiveRevision returns the newest active revision older than rev.
func (s *Store) PreviousActiveRevision(ctx context.Context, rev int64) (int64, error) {
	var prev int64
	err := s.Pool.QueryRow(ctx, `SELECT COALESCE(max(revision), 0) FROM config_revisions
		WHERE status='active' AND target_group='all' AND revision < $1`, rev).Scan(&prev)
	return prev, err
}

// CanaryInFlight returns the newest in-flight canary revision, or 0.
func (s *Store) CanaryInFlight(ctx context.Context) (int64, error) {
	_, canary, err := s.GetRolloutState(ctx)
	return canary, err
}

// CanarySplit is the traffic share a canary revision receives on standard nodes.
type CanarySplit struct {
	TrafficPercent int    `json:"traffic_percent"`
	Header         string `json:"header,omitempty"`
	HeaderValue    string `json:"header_value,omitempty"`
}

// DeployCanary marks rev as the in-flight canary with the given traffic split.
// rev must be the newest active revision (just published) or already the canary.
func (s *Store) DeployCanary(ctx context.Context, rev int64, split CanarySplit) error {
	return pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM config_revisions WHERE revision=$1 FOR UPDATE`, rev).Scan(&status); err != nil {
			return mapErr(err)
		}
		if status == "rolled_back" {
			return fmt.Errorf("%w: revision %d was rolled back and cannot be deployed as a canary", ErrConflict, rev)
		}
		var newer int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(revision), 0) FROM config_revisions
			WHERE revision > $1 AND status IN ('active', 'canary')`, rev).Scan(&newer); err != nil {
			return err
		}
		if newer > 0 {
			return fmt.Errorf("%w: revision %d is newer than %d; only the latest revision can be canaried", ErrConflict, newer, rev)
		}
		var otherCanary int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(revision), 0) FROM config_revisions WHERE status='canary' AND revision <> $1`, rev).Scan(&otherCanary); err != nil {
			return err
		}
		if otherCanary > 0 {
			return fmt.Errorf("%w: revision %d is already the in-flight canary", ErrConflict, otherCanary)
		}
		if _, err := tx.Exec(ctx, `UPDATE config_revisions SET status='canary', target_group='canary',
			traffic_percent=$2, canary_header=$3, canary_header_value=$4 WHERE revision=$1`,
			rev, split.TrafficPercent, split.Header, split.HeaderValue); err != nil {
			return mapErr(err)
		}
		_, err := tx.Exec(ctx, `SELECT pg_notify('relayops_config', json_build_object('table', 'config_revisions', 'op', 'CANARY', 'revision', $1::bigint, 'at', now())::text)`, rev)
		return err
	})
}

// PromoteCanary makes rev the active revision for the whole fleet.
func (s *Store) PromoteCanary(ctx context.Context, rev int64) error {
	return pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM config_revisions WHERE revision=$1 FOR UPDATE`, rev).Scan(&status); err != nil {
			return mapErr(err)
		}
		if status == "rolled_back" {
			return fmt.Errorf("%w: revision %d was rolled back and cannot be promoted", ErrConflict, rev)
		}
		if _, err := tx.Exec(ctx, `UPDATE config_revisions SET status='active', target_group='all',
			traffic_percent=0, canary_header='', canary_header_value='' WHERE revision=$1`, rev); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT pg_notify('relayops_config', json_build_object('table', 'config_revisions', 'op', 'PROMOTE', 'revision', $1::bigint, 'at', now())::text)`, rev)
		return err
	})
}

// AbortCanary withdraws an in-flight canary: every node stops serving it at once,
// then the config tables are restored to the stable revision so the canary's
// changes cannot leak into a later publish. Returns the restoring revision.
func (s *Store) AbortCanary(ctx context.Context, canaryRev int64, actor string) (int64, error) {
	var status string
	if err := s.Pool.QueryRow(ctx, `SELECT status FROM config_revisions WHERE revision=$1`, canaryRev).Scan(&status); err != nil {
		return 0, mapErr(err)
	}
	if status != "canary" {
		return 0, fmt.Errorf("%w: revision %d is not an in-flight canary (status %s)", ErrConflict, canaryRev, status)
	}
	stable, err := s.PreviousActiveRevision(ctx, canaryRev)
	if err != nil {
		return 0, err
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE config_revisions SET status='rolled_back', traffic_percent=0 WHERE revision=$1`, canaryRev); err != nil {
		return 0, err
	}
	_ = s.NotifyRevision(ctx, canaryRev)
	if stable <= 0 {
		return 0, nil
	}
	return s.RollbackConfigRevision(ctx, stable, actor)
}

// MarkRolledBack flags a revision that was replaced by an automatic rollback,
// so it is never chosen as a baseline again.
func (s *Store) MarkRolledBack(ctx context.Context, rev int64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE config_revisions SET status='rolled_back' WHERE revision=$1`, rev)
	return err
}

func assertNoCanary(ctx context.Context, tx pgx.Tx) error {
	var canary int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(revision), 0) FROM config_revisions WHERE status='canary'`).Scan(&canary); err != nil {
		return err
	}
	if canary > 0 {
		return fmt.Errorf("%w (canary rev %d)", ErrCanaryInFlight, canary)
	}
	return nil
}

// IsCanaryInFlight reports whether err is ErrCanaryInFlight.
func IsCanaryInFlight(err error) bool { return errors.Is(err, ErrCanaryInFlight) }

// GetCanarySplit returns the traffic split recorded on a revision.
func (s *Store) GetCanarySplit(ctx context.Context, rev int64) (CanarySplit, error) {
	var sp CanarySplit
	err := s.Pool.QueryRow(ctx, `SELECT traffic_percent, canary_header, canary_header_value FROM config_revisions WHERE revision=$1`, rev).
		Scan(&sp.TrafficPercent, &sp.Header, &sp.HeaderValue)
	return sp, mapErr(err)
}
