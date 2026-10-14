package admin

import (
	"github.com/jackc/pgx/v5"
	"github.com/relayops/apim/internal/store"
	"net/http"
	"time"
)

func (s *Server) requestConsumerSubscription(w http.ResponseWriter, r *http.Request) {
	dev, err := s.authenticateDeveloper(r)
	if err != nil || dev.ID == "admin" {
		writeErr(w, 401, "unauthorized", "use an active application developer key")
		return
	}
	var in struct {
		APIID  string  `json:"api_id"`
		PlanID *string `json:"plan_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	var id string
	err = s.store.WithTx(r.Context(), func(tx pgx.Tx) error {
		var tenant string
		var approval bool
		if err := tx.QueryRow(r.Context(), `SELECT tenant_id,require_approval FROM apis WHERE id=$1 AND enabled AND NOT is_draft AND visibility='public' FOR SHARE`, in.APIID).Scan(&tenant, &approval); err != nil {
			return err
		}
		if tenant != dev.TenantID {
			return store.ErrCrossTenant
		}
		if in.PlanID != nil {
			var pt string
			var selfService bool
			if err := tx.QueryRow(r.Context(), `SELECT tenant_id, self_service FROM plans WHERE id=$1 FOR SHARE`, *in.PlanID).Scan(&pt, &selfService); err != nil {
				return err
			}
			// Developers may only pick a plan offered in the portal.
			if pt != tenant || !selfService {
				return store.ErrCrossTenant
			}
		}
		err := tx.QueryRow(r.Context(), `SELECT id FROM subscriptions WHERE consumer_id=$1 AND api_id=$2`, dev.ID, in.APIID).Scan(&id)
		if err == nil {
			return nil
		}
		if err != pgx.ErrNoRows {
			return err
		}
		status := "approved"
		if approval {
			status = "pending"
		}
		return tx.QueryRow(r.Context(), `INSERT INTO subscriptions(consumer_id,api_id,plan_id,status,active) VALUES($1,$2,$3,$4,$5) ON CONFLICT(consumer_id,api_id) DO UPDATE SET consumer_id=EXCLUDED.consumer_id RETURNING id`, dev.ID, in.APIID, in.PlanID, status, !approval).Scan(&id)
	})
	if err != nil {
		writeErr(w, 400, "subscription_failed", "choose a published API and plan from your application tenant")
		return
	}
	// Reuse the authoritative connection response rather than inferring readiness.
	r.SetPathValue("id", in.APIID)
	s.consumerConnection(w, r)
}

func (s *Server) consumerConnection(w http.ResponseWriter, r *http.Request) {
	dev, err := s.authenticateDeveloper(r)
	if err != nil {
		writeErr(w, 401, "unauthorized", "enter your developer key to resume access setup")
		return
	}
	a, err := s.store.GetAPI(r.Context(), r.PathValue("id"))
	if err != nil || !a.Enabled || a.IsDraft || a.Visibility != "public" {
		writeErr(w, 404, "not_found", "API is not published")
		return
	}
	subs, err := s.store.ListSubscriptions(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	status := "not_requested"
	approved := false
	var planID *string
	for _, sub := range subs {
		if sub.ConsumerID == dev.ID && sub.APIID == a.ID {
			status = sub.Status
			approved = sub.Status == "approved" && sub.Active
			planID = sub.PlanID
			break
		}
	}
	var success *time.Time
	err = s.store.Pool.QueryRow(r.Context(), `SELECT first_success_at FROM consumer_connections WHERE consumer_id=$1 AND api_id=$2`, dev.ID, a.ID).Scan(&success)
	if err != nil && err != pgx.ErrNoRows {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"consumer": dev, "api_id": a.ID, "subscription_status": status, "access_ready": approved, "plan_id": planID, "first_success_at": success})
}
