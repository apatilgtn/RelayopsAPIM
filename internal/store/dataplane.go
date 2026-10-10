package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// DataplaneCredential authenticates one gateway-only node to the node API.
// Only the SHA-256 hash of its token is stored.
type DataplaneCredential struct {
	ID          string     `json:"id"`
	NodeID      string     `json:"node_id"`
	Description string     `json:"description"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	LastSeenAt  *time.Time `json:"last_seen_at,omitempty"`
	LastSeenIP  string     `json:"last_seen_ip"`
}

// DataplaneTokenPrefix marks per-node tokens, so they are recognisable in
// configuration and leak scanners.
const DataplaneTokenPrefix = "rlnode_"

const dataplaneCredCols = `id, node_id, description, created_by, created_at, expires_at, revoked_at, last_seen_at, last_seen_ip`

func scanDataplaneCredential(row pgx.Row) (DataplaneCredential, error) {
	var c DataplaneCredential
	err := row.Scan(&c.ID, &c.NodeID, &c.Description, &c.CreatedBy, &c.CreatedAt, &c.ExpiresAt, &c.RevokedAt, &c.LastSeenAt, &c.LastSeenIP)
	return c, err
}

// CreateDataplaneCredential issues a token for nodeID. The plaintext token is
// returned once and never stored.
func (s *Store) CreateDataplaneCredential(ctx context.Context, nodeID, description, createdBy string, expiresAt *time.Time) (DataplaneCredential, string, error) {
	if nodeID == "" {
		return DataplaneCredential{}, "", errors.New("node_id is required")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return DataplaneCredential{}, "", err
	}
	token := DataplaneTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	c, err := scanDataplaneCredential(s.Pool.QueryRow(ctx, `INSERT INTO dataplane_node_credentials
		(node_id, description, token_hash, created_by, expires_at) VALUES ($1,$2,$3,$4,$5)
		RETURNING `+dataplaneCredCols, nodeID, description, HashKey(token), createdBy, expiresAt))
	if err != nil {
		return DataplaneCredential{}, "", mapErr(err)
	}
	return c, token, nil
}

func (s *Store) ListDataplaneCredentials(ctx context.Context) ([]DataplaneCredential, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+dataplaneCredCols+` FROM dataplane_node_credentials ORDER BY node_id, created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DataplaneCredential{}
	for rows.Next() {
		c, err := scanDataplaneCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RevokeDataplaneCredential revokes a credential; the node is refused on its next call.
func (s *Store) RevokeDataplaneCredential(ctx context.Context, id string) (DataplaneCredential, error) {
	c, err := scanDataplaneCredential(s.Pool.QueryRow(ctx, `UPDATE dataplane_node_credentials
		SET revoked_at = COALESCE(revoked_at, now()) WHERE id = $1 RETURNING `+dataplaneCredCols, id))
	return c, mapErr(err)
}

// AuthenticateDataplaneToken returns the active (unrevoked, unexpired)
// credential for token, or ErrNotFound.
func (s *Store) AuthenticateDataplaneToken(ctx context.Context, token string) (DataplaneCredential, error) {
	c, err := scanDataplaneCredential(s.Pool.QueryRow(ctx, `SELECT `+dataplaneCredCols+` FROM dataplane_node_credentials
		WHERE token_hash = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())`, HashKey(token)))
	return c, mapErr(err)
}

// TouchDataplaneCredential records that the credential was used, at most
// every 30 seconds per credential.
func (s *Store) TouchDataplaneCredential(ctx context.Context, id, ip string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE dataplane_node_credentials SET last_seen_at = now(), last_seen_ip = $2
		WHERE id = $1 AND (last_seen_at IS NULL OR last_seen_at < now() - interval '30 seconds')`, id, ip)
	return err
}
