package scim

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/relayops/apim/internal/store"
)

type UserStore interface {
	ListAdminUsers(ctx context.Context) ([]store.AdminUser, error)
	GetAdminUser(ctx context.Context, id string) (store.AdminUser, error)
	GetAdminUserByEmail(ctx context.Context, email string) (store.AdminUser, error)
	CreateAdminUser(ctx context.Context, u store.AdminUser) (store.AdminUser, error)
	UpdateAdminUser(ctx context.Context, u store.AdminUser) (store.AdminUser, error)
	DeleteAdminUser(ctx context.Context, id string) error
}

type Server struct {
	store       UserStore
	bearerToken string
	baseURL     string
}

func NewServer(st UserStore, bearerToken, baseURL string) *Server {
	return &Server{
		store:       st,
		bearerToken: strings.TrimSpace(bearerToken),
		baseURL:     strings.TrimRight(baseURL, "/"),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ServiceProviderConfig", s.getServiceProviderConfig)
	mux.HandleFunc("GET /ResourceTypes", s.getResourceTypes)
	mux.HandleFunc("GET /Schemas", s.getSchemas)
	mux.HandleFunc("GET /Users", s.listUsers)
	mux.HandleFunc("POST /Users", s.createUser)
	mux.HandleFunc("GET /Users/{id}", s.getUser)
	mux.HandleFunc("PUT /Users/{id}", s.replaceUser)
	mux.HandleFunc("PATCH /Users/{id}", s.patchUser)
	mux.HandleFunc("DELETE /Users/{id}", s.deleteUser)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/scim+json; charset=utf-8")
		if s.bearerToken != "" {
			authHeader := r.Header.Get("Authorization")
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if subtle.ConstantTimeCompare([]byte(token), []byte(s.bearerToken)) != 1 {
				writeSCIMError(w, http.StatusUnauthorized, "invalid or missing bearer token", "")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) getServiceProviderConfig(w http.ResponseWriter, r *http.Request) {
	cfg := ServiceProviderConfig{
		Schemas: []string{ServiceProviderConfigSchema},
		Patch:   SupportedFeature{Supported: true},
		Bulk:    SupportedFeature{Supported: false},
		Filter: FilterFeature{
			Supported:  true,
			MaxResults: 200,
		},
		ChangePassword: SupportedFeature{Supported: false},
		Sort:           SupportedFeature{Supported: false},
		Etag:           SupportedFeature{Supported: false},
		AuthenticationSchemes: []AuthScheme{
			{
				Name:        "OAuth Bearer Token",
				Description: "Authentication using HTTP Bearer Token",
				Type:        "oauthbearertoken",
				Primary:     true,
			},
		},
	}
	_ = json.NewEncoder(w).Encode(cfg)
}

func (s *Server) getResourceTypes(w http.ResponseWriter, r *http.Request) {
	res := []map[string]any{
		{
			"schemas":          []string{ResourceTypeSchema},
			"id":               "User",
			"name":             "User",
			"endpoint":         "/Users",
			"description":      "User Account",
			"schema":           UserSchema,
			"schemaExtensions": []any{},
			"meta": map[string]any{
				"location":     s.baseURL + "/ResourceTypes/User",
				"resourceType": "ResourceType",
			},
		},
	}
	_ = json.NewEncoder(w).Encode(res)
}

func (s *Server) getSchemas(w http.ResponseWriter, r *http.Request) {
	res := []map[string]any{
		{
			"id":          UserSchema,
			"name":        "User",
			"description": "RelayOps Control Plane User Schema",
		},
	}
	_ = json.NewEncoder(w).Encode(res)
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListAdminUsers(r.Context())
	if err != nil {
		writeSCIMError(w, http.StatusInternalServerError, "failed to query users", "")
		return
	}

	filter := strings.TrimSpace(r.URL.Query().Get("filter"))
	filtered := filterUsers(users, filter)

	startIndex := 1
	if sIdx := r.URL.Query().Get("startIndex"); sIdx != "" {
		if val, err := strconv.Atoi(sIdx); err == nil && val > 0 {
			startIndex = val
		}
	}

	count := len(filtered)
	if cStr := r.URL.Query().Get("count"); cStr != "" {
		if val, err := strconv.Atoi(cStr); err == nil && val >= 0 {
			count = val
		}
	}

	total := len(filtered)
	var paginated []store.AdminUser
	start0 := startIndex - 1
	if start0 < total {
		end := start0 + count
		if end > total {
			end = total
		}
		paginated = filtered[start0:end]
	}

	resources := make([]UserResource, 0, len(paginated))
	for _, u := range paginated {
		resources = append(resources, s.toUserResource(u))
	}

	resp := ListResponse{
		Schemas:      []string{ListResponseSchema},
		TotalResults: total,
		StartIndex:   startIndex,
		ItemsPerPage: len(resources),
		Resources:    resources,
	}

	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var in UserResource
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeSCIMError(w, http.StatusBadRequest, "invalid request json: "+err.Error(), "invalidSyntax")
		return
	}

	email := strings.TrimSpace(in.UserName)
	if email == "" && len(in.Emails) > 0 {
		email = strings.TrimSpace(in.Emails[0].Value)
	}
	if email == "" {
		writeSCIMError(w, http.StatusBadRequest, "userName or primary email is required", "invalidValue")
		return
	}

	name := strings.TrimSpace(in.Name.Formatted)
	if name == "" {
		parts := []string{in.Name.GivenName, in.Name.FamilyName}
		name = strings.TrimSpace(strings.Join(parts, " "))
	}
	if name == "" {
		name = email
	}

	role := "developer"
	if len(in.Roles) > 0 && in.Roles[0].Value != "" {
		role = sanitizeRole(in.Roles[0].Value)
	}

	u := store.AdminUser{
		Email:       email,
		Name:        name,
		Role:        role,
		Team:        "SCIM Provisioned",
		Active:      in.Active,
		SSOProvider: "scim",
	}

	created, err := s.store.CreateAdminUser(r.Context(), u)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeSCIMError(w, http.StatusConflict, "user with this email already exists", "uniqueness")
			return
		}
		writeSCIMError(w, http.StatusInternalServerError, "failed to create user: "+err.Error(), "")
		return
	}

	out := s.toUserResource(created)
	w.Header().Set("Location", out.Meta.Location)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	u, err := s.store.GetAdminUser(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeSCIMError(w, http.StatusNotFound, "user not found", "")
			return
		}
		writeSCIMError(w, http.StatusInternalServerError, "failed to get user: "+err.Error(), "")
		return
	}

	_ = json.NewEncoder(w).Encode(s.toUserResource(u))
}

func (s *Server) replaceUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.store.GetAdminUser(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeSCIMError(w, http.StatusNotFound, "user not found", "")
			return
		}
		writeSCIMError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}

	var in UserResource
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeSCIMError(w, http.StatusBadRequest, "invalid request json: "+err.Error(), "invalidSyntax")
		return
	}

	if in.UserName != "" {
		existing.Email = strings.TrimSpace(in.UserName)
	}
	if in.Name.Formatted != "" {
		existing.Name = strings.TrimSpace(in.Name.Formatted)
	}
	if len(in.Roles) > 0 && in.Roles[0].Value != "" {
		existing.Role = sanitizeRole(in.Roles[0].Value)
	}
	existing.Active = in.Active

	updated, err := s.store.UpdateAdminUser(r.Context(), existing)
	if err != nil {
		writeSCIMError(w, http.StatusInternalServerError, "failed to update user: "+err.Error(), "")
		return
	}

	_ = json.NewEncoder(w).Encode(s.toUserResource(updated))
}

func (s *Server) patchUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.store.GetAdminUser(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeSCIMError(w, http.StatusNotFound, "user not found", "")
			return
		}
		writeSCIMError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}

	var req PatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSCIMError(w, http.StatusBadRequest, "invalid patch request json: "+err.Error(), "invalidSyntax")
		return
	}

	for _, op := range req.Operations {
		action := strings.ToLower(op.Op)
		path := strings.ToLower(op.Path)

		switch action {
		case "replace", "add":
			if path == "active" {
				if b, ok := op.Value.(bool); ok {
					existing.Active = b
				}
			} else if path == "roles" || path == "role" {
				if rStr, ok := op.Value.(string); ok {
					existing.Role = sanitizeRole(rStr)
				} else if rArr, ok := op.Value.([]any); ok && len(rArr) > 0 {
					if rMap, ok := rArr[0].(map[string]any); ok {
						if val, ok := rMap["value"].(string); ok {
							existing.Role = sanitizeRole(val)
						}
					}
				}
			} else if path == "name.formatted" || path == "displayname" {
				if n, ok := op.Value.(string); ok {
					existing.Name = n
				}
			} else if path == "" {
				// Object replacement map (e.g. {"active": false})
				if m, ok := op.Value.(map[string]any); ok {
					if b, ok := m["active"].(bool); ok {
						existing.Active = b
					}
					if n, ok := m["name"].(string); ok {
						existing.Name = n
					}
				}
			}
		}
	}

	updated, err := s.store.UpdateAdminUser(r.Context(), existing)
	if err != nil {
		writeSCIMError(w, http.StatusInternalServerError, "failed to update user: "+err.Error(), "")
		return
	}

	_ = json.NewEncoder(w).Encode(s.toUserResource(updated))
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteAdminUser(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeSCIMError(w, http.StatusNotFound, "user not found", "")
			return
		}
		writeSCIMError(w, http.StatusInternalServerError, "failed to delete user: "+err.Error(), "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) toUserResource(u store.AdminUser) UserResource {
	loc := s.baseURL + "/Users/" + u.ID
	if s.baseURL == "" {
		loc = "/scim/v2/Users/" + u.ID
	}
	return UserResource{
		Schemas:  []string{UserSchema},
		ID:       u.ID,
		UserName: u.Email,
		Name: UserName{
			Formatted: u.Name,
		},
		Emails: []UserEmail{
			{Value: u.Email, Type: "work", Primary: true},
		},
		Roles: []UserRole{
			{Value: u.Role, Primary: true},
		},
		Active: u.Active,
		Meta: Meta{
			ResourceType: "User",
			Created:      u.CreatedAt,
			LastModified: u.UpdatedAt,
			Location:     loc,
		},
	}
}

func filterUsers(users []store.AdminUser, filter string) []store.AdminUser {
	if filter == "" {
		return users
	}
	filterLower := strings.ToLower(filter)

	var target string
	for _, prefix := range []string{"username eq \"", "email eq \""} {
		if idx := strings.Index(filterLower, prefix); idx != -1 {
			rem := filter[idx+len(prefix):]
			if end := strings.Index(rem, "\""); end != -1 {
				target = strings.ToLower(rem[:end])
				break
			}
		}
	}

	if target == "" {
		return users
	}

	var res []store.AdminUser
	for _, u := range users {
		if strings.ToLower(u.Email) == target {
			res = append(res, u)
		}
	}
	return res
}

func sanitizeRole(r string) string {
	switch strings.ToLower(strings.TrimSpace(r)) {
	case "superadmin":
		return "superadmin"
	case "admin":
		return "admin"
	case "operator":
		return "operator"
	case "auditor":
		return "auditor"
	default:
		return "developer"
	}
}

func writeSCIMError(w http.ResponseWriter, status int, detail, scimType string) {
	w.Header().Set("Content-Type", "application/scim+json; charset=utf-8")
	w.WriteHeader(status)
	resp := ErrorResponse{
		Schemas:  []string{ErrorSchema},
		Status:   fmt.Sprintf("%d", status),
		Detail:   detail,
		ScimType: scimType,
	}
	_ = json.NewEncoder(w).Encode(resp)
}
