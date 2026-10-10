package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const streamCols = `id, tenant_id, name, description, listen_port, target_addresses,
	tls_mode, max_connections, connect_timeout_ms, enabled, created_at, updated_at`

func scanStream(row pgx.Row) (StreamService, error) {
	var s StreamService
	err := row.Scan(&s.ID, &s.TenantID, &s.Name, &s.Description, &s.ListenPort, &s.TargetAddresses,
		&s.TLSMode, &s.MaxConnections, &s.ConnectTimeoutMS, &s.Enabled, &s.CreatedAt, &s.UpdatedAt)
	if s.TargetAddresses == nil {
		s.TargetAddresses = []string{}
	}
	if s.TLSMode == "" {
		s.TLSMode = "passthrough"
	}
	return s, mapErr(err)
}

func (s *Store) ListStreamServices(ctx context.Context, tenantID string) ([]StreamService, error) {
	var rows pgx.Rows
	var err error
	if tenantID != "" {
		rows, err = s.Pool.Query(ctx, `SELECT `+streamCols+` FROM stream_services WHERE tenant_id=$1 ORDER BY listen_port ASC`, tenantID)
	} else {
		rows, err = s.Pool.Query(ctx, `SELECT `+streamCols+` FROM stream_services ORDER BY listen_port ASC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []StreamService{}
	for rows.Next() {
		srv, err := scanStream(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, srv)
	}
	return out, rows.Err()
}

func (s *Store) GetStreamService(ctx context.Context, id string) (StreamService, error) {
	return scanStream(s.Pool.QueryRow(ctx, `SELECT `+streamCols+` FROM stream_services WHERE id=$1`, id))
}

func (s *Store) CreateStreamService(ctx context.Context, srv StreamService) (StreamService, error) {
	if srv.TargetAddresses == nil {
		srv.TargetAddresses = []string{}
	}
	if srv.TLSMode == "" {
		srv.TLSMode = "passthrough"
	}
	if srv.MaxConnections <= 0 {
		srv.MaxConnections = 1000
	}
	if srv.ConnectTimeoutMS <= 0 {
		srv.ConnectTimeoutMS = 5000
	}
	return scanStream(s.Pool.QueryRow(ctx, `INSERT INTO stream_services
		(tenant_id, name, description, listen_port, target_addresses, tls_mode, max_connections, connect_timeout_ms, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+streamCols,
		TenantOrDefault(srv.TenantID), srv.Name, srv.Description, srv.ListenPort, srv.TargetAddresses,
		srv.TLSMode, srv.MaxConnections, srv.ConnectTimeoutMS, srv.Enabled))
}

func (s *Store) UpdateStreamService(ctx context.Context, srv StreamService) (StreamService, error) {
	if srv.TargetAddresses == nil {
		srv.TargetAddresses = []string{}
	}
	if srv.TLSMode == "" {
		srv.TLSMode = "passthrough"
	}
	if srv.MaxConnections <= 0 {
		srv.MaxConnections = 1000
	}
	if srv.ConnectTimeoutMS <= 0 {
		srv.ConnectTimeoutMS = 5000
	}
	return scanStream(s.Pool.QueryRow(ctx, `UPDATE stream_services SET
		name=$2, description=$3, listen_port=$4, target_addresses=$5, tls_mode=$6,
		max_connections=$7, connect_timeout_ms=$8, enabled=$9, updated_at=now()
		WHERE id=$1 RETURNING `+streamCols,
		srv.ID, srv.Name, srv.Description, srv.ListenPort, srv.TargetAddresses,
		srv.TLSMode, srv.MaxConnections, srv.ConnectTimeoutMS, srv.Enabled))
}

func (s *Store) DeleteStreamService(ctx context.Context, id string) error {
	return s.execOne(ctx, `DELETE FROM stream_services WHERE id=$1`, id)
}
