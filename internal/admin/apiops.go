package admin

import (
	"errors"
	"net/http"

	"github.com/relayops/apim/internal/apiops"
	"github.com/relayops/apim/internal/store"
)

func (s *Server) validateAPIOpsBundle(w http.ResponseWriter, r *http.Request) {
	var in apiops.CompiledBundle
	if !decode(w, r, &in) {
		return
	}
	if err := apiops.ValidateBundle(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_bundle", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"valid":         true,
		"source_hash":   in.SourceHash,
		"rendered_hash": in.RenderedHash,
		"apis_count":    len(in.Config.APIs),
		"plans_count":   len(in.Config.Plans),
	})
}

func (s *Server) planAPIOpsDeployment(w http.ResponseWriter, r *http.Request) {
	tenant, ok := scopeFrom(r).writeTenant(w)
	if !ok {
		return
	}
	var in apiops.PlanRequest
	if !decode(w, r, &in) {
		return
	}
	in.Actor = s.getActor(r)
	if !s.validateAPIOpsCandidate(w, r, tenant, in.EnvironmentID, in.CandidateRevision) {
		return
	}

	res, err := s.apiops.PlanDeployment(r.Context(), tenant, in)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditTenant(r, tenant, "APIOPS_DEPLOYMENT_PLAN", "apiops_deployment", res.Deployment.ID, map[string]any{
		"plan_hash": res.PlanHash, "commit_sha": in.CommitSHA,
	})
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) createAPIOpsDeployment(w http.ResponseWriter, r *http.Request) {
	tenant, ok := scopeFrom(r).writeTenant(w)
	if !ok {
		return
	}
	var in store.APIOpsDeployment
	if !decode(w, r, &in) {
		return
	}
	in.TenantID = tenant
	in.Actor = s.getActor(r)
	in.Status = "planned"
	in.PromotedRevision = 0

	environment := ""
	if in.EnvironmentID != nil {
		environment = *in.EnvironmentID
	}
	if !s.validateAPIOpsCandidate(w, r, tenant, environment, in.CandidateRevision) {
		return
	}

	dep, err := s.store.CreateAPIOpsDeployment(r.Context(), in)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditTenant(r, tenant, "APIOPS_DEPLOYMENT_CREATE", "apiops_deployment", dep.ID, map[string]any{
		"commit_sha": dep.CommitSHA, "status": dep.Status,
	})
	writeJSON(w, http.StatusCreated, dep)
}

func (s *Server) listAPIOpsDeployments(w http.ResponseWriter, r *http.Request) {
	tenant := ""
	if sc := scopeFrom(r); sc.Tenant != nil {
		tenant = sc.Tenant.ID
	}
	deps, err := s.store.ListAPIOpsDeployments(r.Context(), tenant, 50)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deps)
}

func (s *Server) getAPIOpsDeployment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dep, err := s.store.GetAPIOpsDeployment(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "deployment not found")
		return
	}
	if !scopeFrom(r).allows(dep.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "access denied to deployment")
		return
	}
	writeJSON(w, http.StatusOK, dep)
}

func (s *Server) verifyAPIOpsDeployment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dep, err := s.store.GetAPIOpsDeployment(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "deployment not found")
		return
	}
	if !scopeFrom(r).allows(dep.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "access denied to deployment")
		return
	}

	if !scopeFrom(r).Platform {
		writeErr(w, 403, "platform_only", "fleet evidence requires a platform administrator")
		return
	}
	report, reportErr := s.store.EvaluateTestGates(r.Context(), dep.CandidateRevision)
	if reportErr != nil {
		s.fail(w, reportErr)
		return
	}
	digest, err := s.apiops.VerifyDeployment(r.Context(), id)
	if errors.Is(err, apiops.ErrVerificationRejected) {
		writeJSON(w, http.StatusPreconditionFailed, map[string]any{"error": "verification_rejected", "message": err.Error(), "gate_status": report})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditTenant(r, dep.TenantID, "APIOPS_DEPLOYMENT_VERIFY", "apiops_deployment", id, map[string]any{
		"evidence_digest": digest,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"deployment_id":   id,
		"evidence_digest": digest,
		"status":          "verifying",
	})
}

func (s *Server) promoteAPIOpsDeployment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dep, err := s.store.GetAPIOpsDeployment(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "deployment not found")
		return
	}
	if !scopeFrom(r).allows(dep.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "access denied to deployment")
		return
	}

	// Promotion changes a fleet-wide revision, which holds every tenant's configuration.
	if !scopeFrom(r).Platform {
		writeErr(w, http.StatusForbidden, "platform_only", "this operation affects every tenant and requires a platform administrator")
		return
	}

	var req struct {
		OverrideReason string `json:"override_reason"`
		Reviewed       bool   `json:"reviewed"`
	}
	if !decodeOptional(w, r, &req) {
		return
	}

	u, _ := r.Context().Value(userCtxKey{}).(store.AdminUser)
	isSuperadmin := u.Role == "superadmin"

	if req.OverrideReason != "" && !isSuperadmin {
		writeErr(w, http.StatusForbidden, "forbidden", "superadmin role is required to supply a test gate override reason")
		return
	}

	actor := s.getActor(r)
	passport, err := s.apiops.PromoteDeployment(r.Context(), id, actor, isSuperadmin, req.OverrideReason)
	if err != nil {
		writeErr(w, http.StatusPreconditionFailed, "promotion_failed", err.Error())
		return
	}
	s.auditTenant(r, dep.TenantID, "APIOPS_DEPLOYMENT_PROMOTE", "apiops_deployment", id, map[string]any{
		"passport_id":     passport.ID,
		"evidence_digest": passport.EvidenceDigest,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"deployment_id": id,
		"status":        "promoted",
		"passport":      passport,
	})
}

func (s *Server) abortAPIOpsDeployment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dep, err := s.store.GetAPIOpsDeployment(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "deployment not found")
		return
	}
	if !scopeFrom(r).allows(dep.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "access denied to deployment")
		return
	}

	// Aborting withdraws a fleet-wide canary revision.
	if !scopeFrom(r).Platform {
		writeErr(w, http.StatusForbidden, "platform_only", "this operation affects every tenant and requires a platform administrator")
		return
	}

	actor := s.getActor(r)
	if err := s.apiops.AbortDeployment(r.Context(), id, actor); err != nil {
		s.fail(w, err)
		return
	}
	s.auditTenant(r, dep.TenantID, "APIOPS_DEPLOYMENT_ABORT", "apiops_deployment", id, map[string]any{
		"aborted_by": actor,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"deployment_id": id,
		"status":        "aborted",
	})
}

func (s *Server) getAPIOpsPassport(w http.ResponseWriter, r *http.Request) {
	deploymentID := r.PathValue("id")
	passport, err := s.store.GetAPIOpsReleasePassport(r.Context(), deploymentID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "passport_not_found", "release passport not found for deployment")
		return
	}
	if !scopeFrom(r).allows(passport.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "access denied to release passport")
		return
	}
	writeJSON(w, http.StatusOK, passport)
}

func (s *Server) listAPIOpsEnvironments(w http.ResponseWriter, r *http.Request) {
	tenant := ""
	if sc := scopeFrom(r); sc.Tenant != nil {
		tenant = sc.Tenant.ID
	}
	envs, err := s.store.ListAPIOpsEnvironments(r.Context(), tenant)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, envs)
}

func (s *Server) createAPIOpsEnvironment(w http.ResponseWriter, r *http.Request) {
	tenant, ok := scopeFrom(r).writeTenant(w)
	if !ok {
		return
	}
	var in store.APIOpsEnvironment
	if !decode(w, r, &in) {
		return
	}
	in.TenantID = tenant
	if in.Name == "" {
		writeErr(w, http.StatusBadRequest, "invalid_environment", "name is required")
		return
	}
	res, err := s.store.CreateAPIOpsEnvironment(r.Context(), in)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditTenant(r, tenant, "APIOPS_ENV_CREATE", "apiops_environment", res.ID, map[string]any{"name": res.Name})
	writeJSON(w, http.StatusCreated, res)
}

// Revisions contain the whole fleet; they are not owned by a single tenant.
func (s *Server) validateAPIOpsCandidate(w http.ResponseWriter, r *http.Request, tenant, environment string, revision int64) bool {
	if environment != "" {
		env, err := s.store.GetAPIOpsEnvironment(r.Context(), environment)
		if err != nil || env.TenantID != tenant {
			writeErr(w, 400, "invalid_environment", "environment does not belong to tenant")
			return false
		}
	}
	if revision < 0 {
		writeErr(w, 400, "invalid_candidate_revision", "candidate must be a positive revision")
		return false
	}
	if revision > 0 {
		if !scopeFrom(r).Platform {
			writeErr(w, 403, "platform_only", "binding a fleet-wide candidate requires a platform administrator")
			return false
		}
		var status string
		if err := s.store.Pool.QueryRow(r.Context(), `SELECT status FROM config_revisions WHERE revision=$1`, revision).Scan(&status); err != nil || status != "canary" {
			writeErr(w, 400, "invalid_candidate_revision", "candidate must be an in-flight canary")
			return false
		}
	}
	return true
}
