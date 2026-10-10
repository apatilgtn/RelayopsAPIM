package store

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// DefaultTenantID owns everything created before tenants existed, and anything
// created without an explicit tenant.
const DefaultTenantID = "00000000-0000-0000-0000-000000000def"

// TenantOrDefault returns id, or the default tenant when id is empty.
func TenantOrDefault(id string) string {
	if id == "" {
		return DefaultTenantID
	}
	return id
}

// nullableUUID maps "" to SQL NULL for optional UUID columns.
func nullableUUID(id string) any {
	if id == "" {
		return nil
	}
	return id
}

type Tenant struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

var tenantSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// ValidateTenantSlug checks the URL- and header-safe tenant identifier.
func ValidateTenantSlug(slug string) error {
	if !tenantSlugPattern.MatchString(slug) {
		return fmt.Errorf("tenant slug must be 2-63 characters of lowercase letters, digits and '-', starting with a letter or digit")
	}
	return nil
}

const tenantCols = `id, slug, name, created_at, updated_at`

func scanTenant(row pgx.Row) (Tenant, error) {
	var t Tenant
	err := row.Scan(&t.ID, &t.Slug, &t.Name, &t.CreatedAt, &t.UpdatedAt)
	return t, mapErr(err)
}

func (s *Store) ListTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+tenantCols+` FROM tenants ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Tenant{}
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetTenant(ctx context.Context, id string) (Tenant, error) {
	return scanTenant(s.Pool.QueryRow(ctx, `SELECT `+tenantCols+` FROM tenants WHERE id::text=$1`, id))
}

func (s *Store) GetTenantBySlug(ctx context.Context, slug string) (Tenant, error) {
	return scanTenant(s.Pool.QueryRow(ctx, `SELECT `+tenantCols+` FROM tenants WHERE slug=$1`, strings.ToLower(strings.TrimSpace(slug))))
}

func (s *Store) CreateTenant(ctx context.Context, slug, name string) (Tenant, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if err := ValidateTenantSlug(slug); err != nil {
		return Tenant{}, err
	}
	if strings.TrimSpace(name) == "" {
		name = slug
	}
	return scanTenant(s.Pool.QueryRow(ctx, `INSERT INTO tenants (slug, name) VALUES ($1, $2) RETURNING `+tenantCols, slug, strings.TrimSpace(name)))
}

func (s *Store) RenameTenant(ctx context.Context, id, name string) (Tenant, error) {
	return scanTenant(s.Pool.QueryRow(ctx, `UPDATE tenants SET name=$2, updated_at=now() WHERE id::text=$1 RETURNING `+tenantCols, id, strings.TrimSpace(name)))
}

// DeleteTenant removes an empty tenant. A tenant that still owns APIs, plans or
// consumers cannot be deleted, so nothing is removed by accident.
func (s *Store) DeleteTenant(ctx context.Context, id string) error {
	if id == DefaultTenantID {
		return fmt.Errorf("%w: the default tenant cannot be deleted", ErrConflict)
	}
	var owned int
	if err := s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM apis WHERE tenant_id::text=$1) +
		(SELECT count(*) FROM plans WHERE tenant_id::text=$1) + (SELECT count(*) FROM consumers WHERE tenant_id::text=$1)`, id).Scan(&owned); err != nil {
		return err
	}
	if owned > 0 {
		return fmt.Errorf("%w: tenant still owns %d APIs, plans or consumers", ErrConflict, owned)
	}
	return s.execOne(ctx, `DELETE FROM tenants WHERE id::text=$1`, id)
}

// ---------------------------------------------------------------------------
// Memberships
// ---------------------------------------------------------------------------

type Membership struct {
	TenantID   string    `json:"tenant_id"`
	TenantSlug string    `json:"tenant_slug"`
	TenantName string    `json:"tenant_name"`
	UserID     string    `json:"user_id"`
	UserEmail  string    `json:"user_email,omitempty"`
	UserName   string    `json:"user_name,omitempty"`
	Role       string    `json:"role"`
	Source     string    `json:"source"`
	CreatedAt  time.Time `json:"created_at"`
}

var membershipRoles = map[string]bool{"admin": true, "operator": true, "auditor": true, "developer": true}

// ValidMembershipRole reports whether role can be granted within a tenant.
func ValidMembershipRole(role string) bool { return membershipRoles[role] }

const membershipSelect = `SELECT m.tenant_id, t.slug, t.name, m.user_id, u.email, u.name, m.role, m.source, m.created_at
	FROM tenant_memberships m JOIN tenants t ON t.id = m.tenant_id JOIN admin_users u ON u.id = m.user_id`

func (s *Store) queryMemberships(ctx context.Context, where string, args ...any) ([]Membership, error) {
	rows, err := s.Pool.Query(ctx, membershipSelect+` `+where+` ORDER BY t.slug, u.email`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Membership{}
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.TenantID, &m.TenantSlug, &m.TenantName, &m.UserID, &m.UserEmail, &m.UserName, &m.Role, &m.Source, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) ListTenantMembers(ctx context.Context, tenantID string) ([]Membership, error) {
	return s.queryMemberships(ctx, `WHERE m.tenant_id::text=$1`, tenantID)
}

func (s *Store) ListUserMemberships(ctx context.Context, userID string) ([]Membership, error) {
	return s.queryMemberships(ctx, `WHERE m.user_id::text=$1`, userID)
}

// UpsertMembership grants role in tenant to user.
func (s *Store) UpsertMembership(ctx context.Context, tenantID, userID, role, source string) error {
	if !ValidMembershipRole(role) {
		return fmt.Errorf("role must be one of admin, operator, auditor, developer")
	}
	if source == "" {
		source = "manual"
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO tenant_memberships (tenant_id, user_id, role, source) VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, user_id) DO UPDATE SET role=$3, source=$4`, tenantID, userID, role, source)
	return mapErr(err)
}

func (s *Store) DeleteMembership(ctx context.Context, tenantID, userID string) error {
	return s.execOne(ctx, `DELETE FROM tenant_memberships WHERE tenant_id::text=$1 AND user_id::text=$2`, tenantID, userID)
}

// ReplaceSourcedMemberships sets exactly the given memberships for a user from
// one source (e.g. "idp:okta"), leaving memberships from other sources alone.
func (s *Store) ReplaceSourcedMemberships(ctx context.Context, userID, source string, roles map[string]string) error {
	return pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM tenant_memberships WHERE user_id=$1 AND source=$2`, userID, source); err != nil {
			return err
		}
		for tenantID, role := range roles {
			if !ValidMembershipRole(role) {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO tenant_memberships (tenant_id, user_id, role, source) VALUES ($1, $2, $3, $4)
				ON CONFLICT (tenant_id, user_id) DO UPDATE SET role=$3, source=$4`, tenantID, userID, role, source); err != nil {
				return err
			}
		}
		return nil
	})
}

// TenantOfAPIs returns the tenant of each given API ID.
func (s *Store) TenantOfAPIs(ctx context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT id::text, tenant_id::text FROM apis WHERE id::text = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, tenant string
		if err := rows.Scan(&id, &tenant); err != nil {
			return nil, err
		}
		out[id] = tenant
	}
	return out, rows.Err()
}
