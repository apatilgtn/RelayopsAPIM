package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/relayops/apim/internal/certs"
)

// NodeCertificates is the certificate report of one node.
type NodeCertificates struct {
	NodeID       string         `json:"node_id"`
	ReportedAt   time.Time      `json:"reported_at"`
	Certificates []certs.Status `json:"certificates"`
}

// RecordNodeCertificates replaces a node's certificate report.
func (s *Store) RecordNodeCertificates(ctx context.Context, nodeID string, list []certs.Status) error {
	if list == nil {
		list = []certs.Status{}
	}
	if len(list) > certs.MaxReported {
		list = list[:certs.MaxReported]
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO node_certificates (node_id, certificates, reported_at) VALUES ($1, $2, now())
		ON CONFLICT (node_id) DO UPDATE SET certificates = EXCLUDED.certificates, reported_at = now()`, nodeID, raw)
	return err
}

// ListNodeCertificates returns the reports of nodes that acknowledged within
// window. Nodes re-send a report only when it changes, so liveness comes from
// their acknowledgements, not from reported_at.
func (s *Store) ListNodeCertificates(ctx context.Context, window time.Duration) ([]NodeCertificates, error) {
	rows, err := s.Pool.Query(ctx, `SELECT c.node_id, c.reported_at, c.certificates FROM node_certificates c
		WHERE EXISTS (SELECT 1 FROM node_acknowledgements a
			WHERE a.node_id = c.node_id AND a.applied_at > now() - make_interval(secs => $1))
		ORDER BY c.node_id`, window.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NodeCertificates{}
	for rows.Next() {
		var n NodeCertificates
		var raw []byte
		if err := rows.Scan(&n.NodeID, &n.ReportedAt, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &n.Certificates); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ClaimCertificateAlert reports whether this caller should send the alert
// for a certificate at a severity: true at most once a day across every
// control-plane replica.
func (s *Store) ClaimCertificateAlert(ctx context.Context, fingerprint, severity string) (bool, error) {
	var one int
	err := s.Pool.QueryRow(ctx, `INSERT INTO certificate_alerts (fingerprint, severity, alerted_at) VALUES ($1, $2, now())
		ON CONFLICT (fingerprint, severity) DO UPDATE SET alerted_at = now()
		WHERE certificate_alerts.alerted_at < now() - interval '1 day'
		RETURNING 1`, fingerprint, severity).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ACMECache returns the shared ACME cache table as a key-value store.
func (s *Store) ACMECache() certs.KV { return acmeKV{s} }

type acmeKV struct{ s *Store }

func (k acmeKV) Get(ctx context.Context, key string) ([]byte, bool, error) {
	var data []byte
	err := k.s.Pool.QueryRow(ctx, `SELECT data FROM acme_cache WHERE cache_key = $1`, key).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return data, err == nil, err
}

func (k acmeKV) Put(ctx context.Context, key string, data []byte) error {
	_, err := k.s.Pool.Exec(ctx, `INSERT INTO acme_cache (cache_key, data, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (cache_key) DO UPDATE SET data = EXCLUDED.data, updated_at = now()`, key, data)
	return err
}

func (k acmeKV) Delete(ctx context.Context, key string) error {
	_, err := k.s.Pool.Exec(ctx, `DELETE FROM acme_cache WHERE cache_key = $1`, key)
	return err
}

// TryACMEOrderLease reports whether holder may order host's certificate now:
// true when no other holder has an unexpired lease. The holder renews its
// own lease.
func (s *Store) TryACMEOrderLease(ctx context.Context, host, holder string, ttl time.Duration) (bool, error) {
	var got string
	err := s.Pool.QueryRow(ctx, `INSERT INTO acme_order_leases (host, holder, expires_at)
		VALUES ($1, $2, now() + make_interval(secs => $3))
		ON CONFLICT (host) DO UPDATE SET holder = EXCLUDED.holder, expires_at = EXCLUDED.expires_at
		WHERE acme_order_leases.expires_at < now() OR acme_order_leases.holder = EXCLUDED.holder
		RETURNING holder`, host, holder, ttl.Seconds()).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
