package store

import (
	"context"
	"encoding/json"
	"time"
)

// MCPCatalogEntry is one tool or prompt definition seen on an MCP API.
type MCPCatalogEntry struct {
	ID          string          `json:"id"`
	TenantID    string          `json:"tenant_id"`
	APIID       string          `json:"api_id"`
	APIName     string          `json:"api_name,omitempty"`
	Kind        string          `json:"kind"` // tool, prompt
	Name        string          `json:"name"`
	Fingerprint string          `json:"fingerprint"`
	Definition  json.RawMessage `json:"definition"`
	Status      string          `json:"status"` // pending, approved, rejected, resolved
	Source      string          `json:"source"` // observed, discovered
	FirstSeen   time.Time       `json:"first_seen"`
	LastSeen    time.Time       `json:"last_seen"`
	SeenBy      string          `json:"seen_by"`
	DecidedBy   string          `json:"decided_by,omitempty"`
	DecidedAt   *time.Time      `json:"decided_at,omitempty"`
	// Pinned is the API's currently approved fingerprint for this name.
	Pinned string `json:"pinned,omitempty"`
}

// MCPObservation is a definition a gateway or discovery reports.
type MCPObservation struct {
	APIID       string          `json:"api_id"`
	Kind        string          `json:"kind"`
	Name        string          `json:"name"`
	Fingerprint string          `json:"fingerprint"`
	Definition  json.RawMessage `json:"definition"`
	// Status for a new entry: pending (needs review), approved (it matches
	// the current pin) or resolved (recorded only; the API has no approved
	// catalog for this kind).
	Status string `json:"status"`
	Source string `json:"source"`
}

// MCPCatalogRecord is the catalog state gateways enforce: pending and
// rejected definitions are blocked, approved tool definitions carry the
// input schema for argument validation.
type MCPCatalogRecord struct {
	APIID       string          `json:"api_id"`
	Kind        string          `json:"kind"`
	Name        string          `json:"name"`
	Fingerprint string          `json:"fingerprint"`
	Status      string          `json:"status"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

const mcpCatalogCols = `c.id, c.tenant_id, c.api_id, a.name, c.kind, c.name, c.fingerprint, c.definition, c.status, c.source,
	c.first_seen, c.last_seen, c.seen_by, c.decided_by, c.decided_at`

func scanMCPCatalog(row interface{ Scan(...any) error }) (MCPCatalogEntry, error) {
	var e MCPCatalogEntry
	err := row.Scan(&e.ID, &e.TenantID, &e.APIID, &e.APIName, &e.Kind, &e.Name, &e.Fingerprint, &e.Definition, &e.Status,
		&e.Source, &e.FirstSeen, &e.LastSeen, &e.SeenBy, &e.DecidedBy, &e.DecidedAt)
	return e, mapErr(err)
}

// RecordMCPObservations stores observed definitions and returns the ones
// that are new or newly awaiting review. Known definitions only refresh
// last_seen, which does not trigger a gateway reload.
func (s *Store) RecordMCPObservations(ctx context.Context, obs []MCPObservation, seenBy string) ([]MCPCatalogEntry, error) {
	var created []MCPCatalogEntry
	for _, o := range obs {
		if o.Status != "approved" && o.Status != "resolved" {
			o.Status = "pending"
		}
		if o.Source != "discovered" {
			o.Source = "observed"
		}
		var id string
		var inserted, promoted bool
		// A definition recorded only for information (status resolved, nobody
		// decided on it) becomes pending once the API has an approved catalog.
		err := s.Pool.QueryRow(ctx, `WITH prev AS (
				SELECT status, decided_by FROM mcp_catalog WHERE api_id = $1 AND kind = $2 AND name = $3 AND fingerprint = $4)
			INSERT INTO mcp_catalog (tenant_id, api_id, kind, name, fingerprint, definition, status, source, seen_by)
			SELECT tenant_id, id, $2, $3, $4, $5, $6, $7, $8 FROM apis WHERE id = $1
			ON CONFLICT (api_id, kind, name, fingerprint) DO UPDATE SET last_seen = now(), seen_by = EXCLUDED.seen_by,
				status = CASE WHEN mcp_catalog.status = 'resolved' AND mcp_catalog.decided_by = '' AND EXCLUDED.status = 'pending'
					THEN 'pending' ELSE mcp_catalog.status END
			RETURNING id, (xmax = 0),
				COALESCE((SELECT status = 'resolved' AND decided_by = '' FROM prev), false) AND status = 'pending'`,
			o.APIID, o.Kind, o.Name, o.Fingerprint, o.Definition, o.Status, o.Source, seenBy).Scan(&id, &inserted, &promoted)
		if err != nil {
			if mapErr(err) == ErrNotFound {
				continue // the API was deleted
			}
			return created, mapErr(err)
		}
		inserted = inserted || promoted
		if inserted {
			e, err := s.GetMCPCatalogEntry(ctx, id)
			if err != nil {
				return created, err
			}
			created = append(created, e)
		}
	}
	return created, nil
}

func (s *Store) GetMCPCatalogEntry(ctx context.Context, id string) (MCPCatalogEntry, error) {
	return scanMCPCatalog(s.Pool.QueryRow(ctx, `SELECT `+mcpCatalogCols+` FROM mcp_catalog c JOIN apis a ON a.id = c.api_id WHERE c.id = $1`, id))
}

// ListMCPCatalog lists entries, newest first. Empty apiID or status means
// all; tenantIDs nil means every tenant.
func (s *Store) ListMCPCatalog(ctx context.Context, tenantIDs []string, apiID, status string) ([]MCPCatalogEntry, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+mcpCatalogCols+` FROM mcp_catalog c JOIN apis a ON a.id = c.api_id
		WHERE ($1::text[] IS NULL OR c.tenant_id::text = ANY($1))
		  AND ($2 = '' OR c.api_id::text = $2) AND ($3 = '' OR c.status = $3)
		ORDER BY c.first_seen DESC LIMIT 500`, tenantIDs, apiID, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MCPCatalogEntry{}
	for rows.Next() {
		e, err := scanMCPCatalog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetMCPCatalogStatus records a review decision.
func (s *Store) SetMCPCatalogStatus(ctx context.Context, id, status, actor string) error {
	return s.execOne(ctx, `UPDATE mcp_catalog SET status = $2, decided_by = $3, decided_at = now() WHERE id = $1`, id, status, actor)
}

// SyncMCPApprovals marks the entries matching an API's pins as approved, so
// approvals made by editing the policy (console, API, APIOps) show in the
// catalog too.
func (s *Store) SyncMCPApprovals(ctx context.Context, apiID, kind string, pins map[string]string, actor string) error {
	for name, fp := range pins {
		if _, err := s.Pool.Exec(ctx, `UPDATE mcp_catalog SET status = 'approved', decided_by = $5, decided_at = now()
			WHERE api_id = $1 AND kind = $2 AND name = $3 AND fingerprint = $4 AND status <> 'approved'`,
			apiID, kind, name, fp, actor); err != nil {
			return err
		}
	}
	return nil
}
