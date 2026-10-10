package admin

import (
	"errors"
	"io/fs"
	"net/http"
	"net/mail"
	"strings"

	"github.com/relayops/apim/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func (s *Server) serveAuthPage(w http.ResponseWriter, r *http.Request) {
	page, err := fs.ReadFile(s.static, "auth.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(page)
}

// Signup requests require approval by an existing platform administrator.
// No session or privileged access is granted by submitting this form.
func (s *Server) authSignup(w http.ResponseWriter, r *http.Request) {
	if !s.regLimiter.Allow("signup:"+clientIP(r), 5).Allowed {
		w.Header().Set("Retry-After", "60")
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "Please wait a minute before trying again.")
		return
	}
	var in struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Team     string `json:"team"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Team = strings.TrimSpace(in.Team)
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	address, err := mail.ParseAddress(in.Email)
	if err != nil || address.Address != in.Email || len(in.Email) > 254 || len(in.Name) < 2 || len(in.Name) > 120 || len(in.Team) < 2 || len(in.Team) > 120 || len(in.Password) < 12 || len(in.Password) > 72 {
		writeErr(w, http.StatusBadRequest, "invalid_signup", "Enter your name, a valid email, your team, and a password of 12–72 bytes.")
		return
	}
	if !s.dbAvailable() {
		writeDBUnavailable(w)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		s.fail(w, err)
		return
	}
	_, lookupErr := s.store.GetAdminUserByEmail(r.Context(), in.Email)
	if lookupErr != nil && !errors.Is(lookupErr, store.ErrNotFound) {
		s.fail(w, lookupErr)
		return
	}
	if errors.Is(lookupErr, store.ErrNotFound) {
		user, err := s.store.CreateAdminUser(r.Context(), store.AdminUser{Name: in.Name, Email: in.Email, Team: in.Team, PasswordHash: string(hash), Role: "auditor", Active: false, SSOProvider: "local"})
		if err != nil && !errors.Is(err, store.ErrConflict) {
			s.fail(w, err)
			return
		}
		if err == nil {
			s.audit(r, "REQUEST_ACCESS", "admin_user", user.ID, map[string]any{"name": user.Name, "email": user.Email})
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending_approval", "message": "If this email is eligible, your access request is awaiting administrator approval. No workspace access is granted until approval."})
}

func verifyAccountPassword(hash, password string) bool {
	return strings.HasPrefix(hash, "$2") && bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func hashAccountPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}
