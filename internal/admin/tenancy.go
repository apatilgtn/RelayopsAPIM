package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/relayops/apim/internal/store"
)

var errNoRows = pgx.ErrNoRows

// TenantHeader selects the tenant an administrator is acting in.
const TenantHeader = "X-RelayOps-Tenant"

// tenantScope is the tenant boundary of one admin request.
//
//   - Platform administrators (superadmin, or any administrator without tenant
//     memberships) see every tenant. With X-RelayOps-Tenant they act inside that
//     tenant; without it, reads span all tenants and creates go to "default".
//   - Tenant-scoped administrators (with memberships) only ever see their own
//     tenants, with the role held in the selected tenant.
type tenantScope struct {
	Platform    bool
	All         bool     // reads span every tenant
	TenantIDs   []string // tenants visible to reads when !All
	WriteTenant string   // tenant that creates and declarative applies target ("" = none selected)
	Tenant      *store.Tenant
	Memberships []store.Membership
}

type scopeCtxKey struct{}

func scopeFrom(r *http.Request) tenantScope {
	if sc, ok := r.Context().Value(scopeCtxKey{}).(tenantScope); ok {
		return sc
	}
	return tenantScope{Platform: true, All: true, WriteTenant: store.DefaultTenantID}
}

// allows reports whether a resource owned by tenantID is visible in this scope.
func (sc tenantScope) allows(tenantID string) bool {
	if sc.All {
		return true
	}
	tenantID = store.TenantOrDefault(tenantID)
	for _, id := range sc.TenantIDs {
		if id == tenantID {
			return true
		}
	}
	return false
}

// filter returns the tenant filter for queries (nil = all tenants).
func (sc tenantScope) filter() []string {
	if sc.All {
		return nil
	}
	return sc.TenantIDs
}

// writeTenant returns the tenant for creates, or writes a 400 when the caller
// has several tenants and did not select one.
func (sc tenantScope) writeTenant(w http.ResponseWriter) (string, bool) {
	if sc.WriteTenant == "" {
		writeErr(w, http.StatusBadRequest, "tenant_required",
			"select the tenant to act in with the "+TenantHeader+" header ("+strings.Join(sc.memberSlugs(), ", ")+")")
		return "", false
	}
	return sc.WriteTenant, true
}

func (sc tenantScope) memberSlugs() []string {
	var out []string
	for _, m := range sc.Memberships {
		out = append(out, m.TenantSlug)
	}
	return out
}

var roleRank = map[string]int{"developer": 1, "auditor": 2, "operator": 3, "admin": 4, "superadmin": 5}

// resolveTenantScope works out the request's tenant boundary and the
// administrator's effective role inside it.
func (s *Server) resolveTenantScope(r *http.Request, user store.AdminUser) (store.AdminUser, tenantScope, int, error) {
	slug := strings.ToLower(strings.TrimSpace(r.Header.Get(TenantHeader)))
	platform := tenantScope{Platform: true, All: true, WriteTenant: store.DefaultTenantID}
	if !s.dbAvailable() {
		return user, platform, 0, nil
	}
	var memberships []store.Membership
	if user.ID != "" && user.ID != clusterAdminFallbackID {
		var err error
		if memberships, err = s.store.ListUserMemberships(r.Context(), user.ID); err != nil {
			return user, platform, http.StatusInternalServerError, err
		}
	}

	if user.Role == "superadmin" || len(memberships) == 0 {
		if slug == "" {
			return user, platform, 0, nil
		}
		t, err := s.store.GetTenantBySlug(r.Context(), slug)
		if err != nil {
			return user, platform, http.StatusBadRequest, fmt.Errorf("unknown tenant %q", slug)
		}
		return user, tenantScope{Platform: true, TenantIDs: []string{t.ID}, WriteTenant: t.ID, Tenant: &t}, 0, nil
	}

	sc := tenantScope{Memberships: memberships}
	if slug != "" {
		for _, m := range memberships {
			if m.TenantSlug == slug {
				t := store.Tenant{ID: m.TenantID, Slug: m.TenantSlug, Name: m.TenantName}
				sc.TenantIDs, sc.WriteTenant, sc.Tenant = []string{m.TenantID}, m.TenantID, &t
				user.Role = m.Role
				return user, sc, 0, nil
			}
		}
		return user, sc, http.StatusForbidden, fmt.Errorf("not a member of tenant %q", slug)
	}
	if len(memberships) == 1 {
		m := memberships[0]
		t := store.Tenant{ID: m.TenantID, Slug: m.TenantSlug, Name: m.TenantName}
		sc.TenantIDs, sc.WriteTenant, sc.Tenant = []string{m.TenantID}, m.TenantID, &t
		user.Role = m.Role
		return user, sc, 0, nil
	}
	// Several tenants and none selected: reads span all of them with the least
	// privileged role held; creates need an explicit selection.
	user.Role = memberships[0].Role
	for _, m := range memberships {
		sc.TenantIDs = append(sc.TenantIDs, m.TenantID)
		if roleRank[m.Role] < roleRank[user.Role] {
			user.Role = m.Role
		}
	}
	return user, sc, 0, nil
}

// platformOnly lists routes that act on the whole fleet or on identity, and so
// are closed to tenant-scoped administrators. Revision snapshots, for example,
// contain every tenant's configuration.
func platformOnly(path string) bool {
	for _, p := range []string{"/api/revisions", "/api/system/drift", "/api/admin/", "/api/upstreams/", "/api/stream"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// tenantOfLog returns the tenant a request log belongs to.
func (s *Server) tenantOfLog(ctx context.Context, l store.RequestLog) string {
	if l.TenantID != "" {
		return l.TenantID
	}
	if l.APIID != nil {
		if m, err := s.store.TenantOfAPIs(ctx, []string{*l.APIID}); err == nil {
			return m[*l.APIID]
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Scoped resource access helpers (respond 404 outside the caller's tenants, so
// other tenants' resources are indistinguishable from missing ones)
// ---------------------------------------------------------------------------

func (s *Server) scopedAPI(w http.ResponseWriter, r *http.Request, id string) (store.API, bool) {
	a, err := s.store.GetAPI(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return a, false
	}
	if !scopeFrom(r).allows(a.TenantID) {
		s.fail(w, store.ErrNotFound)
		return a, false
	}
	return a, true
}

func (s *Server) scopedPlan(w http.ResponseWriter, r *http.Request, id string) (store.Plan, bool) {
	p, err := s.store.GetPlan(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return p, false
	}
	if !scopeFrom(r).allows(p.TenantID) {
		s.fail(w, store.ErrNotFound)
		return p, false
	}
	return p, true
}

func (s *Server) scopedConsumer(w http.ResponseWriter, r *http.Request, id string) (store.Consumer, bool) {
	c, err := s.store.GetConsumer(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return c, false
	}
	if !scopeFrom(r).allows(c.TenantID) {
		s.fail(w, store.ErrNotFound)
		return c, false
	}
	return c, true
}

func (s *Server) scopedKey(w http.ResponseWriter, r *http.Request, id string) (store.APIKey, bool) {
	k, err := s.store.GetAPIKey(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return k, false
	}
	if _, ok := s.scopedConsumer(w, r, k.ConsumerID); !ok {
		return k, false
	}
	return k, true
}

func (s *Server) scopedSub(w http.ResponseWriter, r *http.Request, id string) (store.Subscription, bool) {
	x, err := s.store.GetSubscription(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return x, false
	}
	if !scopeFrom(r).allows(x.TenantID) {
		s.fail(w, store.ErrNotFound)
		return x, false
	}
	return x, true
}

// ---------------------------------------------------------------------------
// Tenant and membership administration
// ---------------------------------------------------------------------------

// canManageMembers: platform superadmins/admins everywhere; tenant admins in their tenant.
func canManageMembers(r *http.Request, tenantID string) bool {
	user, _ := r.Context().Value(userCtxKey{}).(store.AdminUser)
	sc := scopeFrom(r)
	if sc.Platform {
		return user.Role == "superadmin" || user.Role == "admin"
	}
	for _, m := range sc.Memberships {
		if m.TenantID == tenantID && m.Role == "admin" {
			return true
		}
	}
	return false
}

func isPlatformAdmin(r *http.Request) bool {
	user, _ := r.Context().Value(userCtxKey{}).(store.AdminUser)
	return scopeFrom(r).Platform && (user.Role == "superadmin" || user.Role == "admin")
}

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.ListTenants(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	sc := scopeFrom(r)
	out := []store.Tenant{}
	for _, t := range all {
		if sc.Platform || sc.allows(t.ID) || memberOf(sc, t.ID) {
			out = append(out, t)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func memberOf(sc tenantScope, tenantID string) bool {
	for _, m := range sc.Memberships {
		if m.TenantID == tenantID {
			return true
		}
	}
	return false
}

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	if !isPlatformAdmin(r) {
		writeErr(w, http.StatusForbidden, "forbidden", "creating tenants requires a platform superadmin or admin")
		return
	}
	var in struct {
		Slug       string `json:"slug"`
		Name       string `json:"name"`
		AdminEmail string `json:"admin_email"` // optional first tenant administrator
	}
	if !decode(w, r, &in) {
		return
	}
	if s.license.Enforced && s.license.MaxTenants > 0 {
		tenants, err := s.store.ListTenants(r.Context())
		if err != nil {
			s.fail(w, err)
			return
		}
		if s.license.TenantLimitReached(len(tenants)) {
			writeErr(w, http.StatusPaymentRequired, "license_limit",
				fmt.Sprintf("edition %s allows %d tenants", s.license.Edition, s.license.MaxTenants))
			return
		}
	}
	t, err := s.store.CreateTenant(r.Context(), in.Slug, in.Name)
	if err != nil {
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			s.fail(w, err)
		} else {
			writeErr(w, http.StatusBadRequest, "validation_failed", err.Error())
		}
		return
	}
	var member *store.Membership
	if strings.TrimSpace(in.AdminEmail) != "" {
		m, status, err := s.addMember(r, t, in.AdminEmail, "admin")
		if err != nil {
			writeErr(w, status, "member_failed", "tenant created, but adding its administrator failed: "+err.Error())
			return
		}
		member = &m
	}
	s.auditTenant(r, t.ID, "CREATE", "tenant", t.ID, map[string]any{"name": t.Slug, "admin_email": in.AdminEmail})
	writeJSON(w, http.StatusCreated, map[string]any{"tenant": t, "admin": member})
}

func (s *Server) tenantByPath(w http.ResponseWriter, r *http.Request) (store.Tenant, bool) {
	key := r.PathValue("tenant")
	t, err := s.store.GetTenantBySlug(r.Context(), key)
	if err != nil {
		t, err = s.store.GetTenant(r.Context(), key)
	}
	sc := scopeFrom(r)
	if err != nil || !(sc.Platform || memberOf(sc, t.ID)) {
		s.fail(w, store.ErrNotFound)
		return t, false
	}
	return t, true
}

func (s *Server) renameTenant(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tenantByPath(w, r)
	if !ok {
		return
	}
	if !canManageMembers(r, t.ID) {
		writeErr(w, http.StatusForbidden, "forbidden", "renaming a tenant requires its admin role or a platform admin")
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		writeErr(w, http.StatusBadRequest, "validation_failed", "name is required")
		return
	}
	out, err := s.store.RenameTenant(r.Context(), t.ID, in.Name)
	if err == nil {
		s.auditTenant(r, t.ID, "UPDATE", "tenant", t.ID, map[string]any{"name": in.Name})
	}
	s.respond(w, http.StatusOK, out, err)
}

func (s *Server) deleteTenant(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tenantByPath(w, r)
	if !ok {
		return
	}
	if !isPlatformAdmin(r) {
		writeErr(w, http.StatusForbidden, "forbidden", "deleting tenants requires a platform superadmin or admin")
		return
	}
	err := s.store.DeleteTenant(r.Context(), t.ID)
	if err == nil {
		s.auditTenant(r, t.ID, "DELETE", "tenant", t.ID, map[string]any{"name": t.Slug})
	}
	s.noContent(w, err)
}

func (s *Server) listTenantMembers(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tenantByPath(w, r)
	if !ok {
		return
	}
	members, err := s.store.ListTenantMembers(r.Context(), t.ID)
	s.respond(w, http.StatusOK, members, err)
}

// addTenantMember onboards a person into a tenant by email. If they have no
// account yet, one is created without a password: they sign in through the
// tenant's SSO provider (or an administrator sets a password).
func (s *Server) addTenantMember(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tenantByPath(w, r)
	if !ok {
		return
	}
	if !canManageMembers(r, t.ID) {
		writeErr(w, http.StatusForbidden, "forbidden", "managing members requires the tenant's admin role or a platform admin")
		return
	}
	var in struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if !decode(w, r, &in) {
		return
	}
	m, status, err := s.addMember(r, t, in.Email, in.Role)
	if err != nil {
		writeErr(w, status, "validation_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) addMember(r *http.Request, t store.Tenant, email, role string) (store.Membership, int, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") || len(email) > 254 {
		return store.Membership{}, http.StatusBadRequest, errors.New("a valid email is required")
	}
	if !store.ValidMembershipRole(role) {
		return store.Membership{}, http.StatusBadRequest, errors.New("role must be one of admin, operator, auditor, developer")
	}
	u, err := s.store.GetAdminUserByEmail(r.Context(), email)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, errNoRows) {
			return store.Membership{}, http.StatusInternalServerError, err
		}
		// Tenant members get the least privileged platform role; their real
		// permissions come from the membership.
		u, err = s.store.CreateAdminUser(r.Context(), store.AdminUser{Email: email, Name: email, Role: "developer", Team: t.Name, SSOProvider: "invited"})
		if err != nil {
			return store.Membership{}, http.StatusInternalServerError, err
		}
	}
	if u.Role == "superadmin" {
		return store.Membership{}, http.StatusBadRequest, errors.New("superadmins already have access to every tenant")
	}
	if err := s.store.UpsertMembership(r.Context(), t.ID, u.ID, role, "manual"); err != nil {
		return store.Membership{}, http.StatusBadRequest, err
	}
	s.auditTenant(r, t.ID, "ADD_MEMBER", "tenant", t.ID, map[string]any{"name": email, "role": role})
	return store.Membership{TenantID: t.ID, TenantSlug: t.Slug, TenantName: t.Name, UserID: u.ID, UserEmail: u.Email, UserName: u.Name, Role: role, Source: "manual"}, 0, nil
}

func (s *Server) removeTenantMember(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tenantByPath(w, r)
	if !ok {
		return
	}
	if !canManageMembers(r, t.ID) {
		writeErr(w, http.StatusForbidden, "forbidden", "managing members requires the tenant's admin role or a platform admin")
		return
	}
	userID := r.PathValue("user")
	err := s.store.DeleteMembership(r.Context(), t.ID, userID)
	if err == nil {
		// Their sessions end so the removal takes effect immediately.
		_ = s.store.ExpireAdminSessionsForUser(r.Context(), userID)
		s.auditTenant(r, t.ID, "REMOVE_MEMBER", "tenant", t.ID, map[string]any{"user_id": userID})
	}
	s.noContent(w, err)
}

func (s *Server) auditTenant(r *http.Request, tenantID, action, resType, resID string, details map[string]any) {
	s.auditWithStateTenant(r, tenantID, action, resType, resID, details, nil, nil, nil)
}

// sortedKeys is a small helper for deterministic output.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
