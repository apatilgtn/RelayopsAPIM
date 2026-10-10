package admin

import (
	"net/http"
	"time"

	"github.com/relayops/apim/internal/store"
)

// Per-node credentials for gateway-only nodes. Issuing one hands out access
// to every tenant's configuration, so only superadmins manage them.

func requireSuperadmin(w http.ResponseWriter, r *http.Request) bool {
	user, _ := r.Context().Value(userCtxKey{}).(store.AdminUser)
	if user.Role != "superadmin" {
		writeErr(w, http.StatusForbidden, "forbidden", "managing gateway node credentials requires a superadmin")
		return false
	}
	return true
}

func (s *Server) listDataplaneCredentials(w http.ResponseWriter, r *http.Request) {
	if !requireSuperadmin(w, r) {
		return
	}
	creds, err := s.store.ListDataplaneCredentials(r.Context())
	s.respond(w, http.StatusOK, creds, err)
}

func (s *Server) createDataplaneCredential(w http.ResponseWriter, r *http.Request) {
	if !requireSuperadmin(w, r) {
		return
	}
	var in struct {
		NodeID        string `json:"node_id"`
		Description   string `json:"description"`
		ExpiresInDays int    `json:"expires_in_days"` // 0 = no expiry
	}
	if !decode(w, r, &in) {
		return
	}
	if in.NodeID == "" || len(in.NodeID) > 253 {
		writeErr(w, http.StatusBadRequest, "validation_failed", "node_id is required (the gateway's RELAYOPS_NODE_ID)")
		return
	}
	if in.ExpiresInDays < 0 {
		writeErr(w, http.StatusBadRequest, "validation_failed", "expires_in_days must not be negative")
		return
	}
	var expires *time.Time
	if in.ExpiresInDays > 0 {
		t := time.Now().Add(time.Duration(in.ExpiresInDays) * 24 * time.Hour)
		expires = &t
	}
	cred, token, err := s.store.CreateDataplaneCredential(r.Context(), in.NodeID, in.Description, s.getActor(r), expires)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "DATAPLANE_CREDENTIAL_CREATE", "dataplane_credential", cred.ID, map[string]any{
		"node_id": cred.NodeID, "expires_at": cred.ExpiresAt,
	})
	// The token is shown once; only its hash is stored.
	writeJSON(w, http.StatusCreated, map[string]any{"credential": cred, "token": token})
}

// RuntimeInfo describes how this node was started, for the console.
type RuntimeInfo struct {
	Role          string
	LogSampleRate float64
}

// WithRuntimeInfo records startup settings shown by GET /api/system/runtime.
func WithRuntimeInfo(ri RuntimeInfo) Option {
	return func(s *Server) { s.runtime = ri }
}

// systemRuntime reports operational settings (no secrets): role, request-log
// sampling, supervisor interval and how the node API authenticates and signs.
func (s *Server) systemRuntime(w http.ResponseWriter, r *http.Request) {
	interval := s.autoRollbackInterval
	if interval <= 0 {
		interval = DefaultAutoRollbackInterval
	}
	rate := s.runtime.LogSampleRate
	if rate <= 0 || rate > 1 {
		rate = 1
	}
	role := s.runtime.Role
	if role == "" {
		role = "all"
	}
	nodeAPI := map[string]any{"enabled": s.dataplane != nil}
	if d := s.dataplane; d != nil {
		nodeAPI["shared_token"] = len(d.token) > 0
		nodeAPI["require_client_cert"] = d.requireClientCert
		if d.signer != nil {
			nodeAPI["signing_key_id"] = d.signer.KeyID()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"node_id":                        s.nodeID,
		"role":                           role,
		"log_sample_rate":                rate,
		"auto_rollback_interval_seconds": interval.Seconds(),
		"node_api":                       nodeAPI,
	})
}

func (s *Server) revokeDataplaneCredential(w http.ResponseWriter, r *http.Request) {
	if !requireSuperadmin(w, r) {
		return
	}
	cred, err := s.store.RevokeDataplaneCredential(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "DATAPLANE_CREDENTIAL_REVOKE", "dataplane_credential", cred.ID, map[string]any{"node_id": cred.NodeID})
	writeJSON(w, http.StatusOK, cred)
}
