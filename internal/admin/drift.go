package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/relayops/apim/internal/store"
)

// driftLockKey elects the single control-plane node that adopts direct SQL edits.
const driftLockKey int64 = 0x52454c4159445246 // "RELAYDRF"

// adoptConfigDrift publishes direct edits to the apis/plans tables as a new
// revision, so changes made in SQL are served, versioned, audited and
// reversible like any other. Drift is not adopted while a canary is in flight
// (the publish guard would refuse it); it is picked up after promote or abort.
func (s *Server) adoptConfigDrift(ctx context.Context) (store.ConfigDrift, int64, error) {
	var drift store.ConfigDrift
	var rev int64
	var adoptErr error
	_, err := s.store.WithAdvisoryLock(ctx, driftLockKey, func(ctx context.Context) error {
		drift, adoptErr = s.store.DetectConfigDrift(ctx)
		if adoptErr != nil || !drift.Drifted {
			return nil
		}
		if canary, _ := s.store.CanaryInFlight(ctx); canary > 0 {
			adoptErr = fmt.Errorf("direct database change detected (%s) but canary rev_%d is in flight; it will be adopted after promote or abort",
				strings.Join(append(drift.ChangedAPIs, drift.ChangedPlans...), ", "), canary)
			return nil
		}
		desc := "Adopted direct database change: " + strings.Join(append(prefixed("api ", drift.ChangedAPIs), prefixed("plan ", drift.ChangedPlans)...), ", ")
		if len(desc) > 500 {
			desc = desc[:497] + "..."
		}
		rev, adoptErr = s.store.AtomicPublishConfig(ctx, "direct_sql", desc, "all", "active", nil)
		if adoptErr != nil {
			return nil
		}
		details, _ := json.Marshal(drift)
		var m map[string]any
		_ = json.Unmarshal(details, &m)
		m["revision"] = rev
		_ = s.store.CreateRichAuditLog(ctx, store.AuditLog{
			Actor: "direct_sql", ActorRole: "system", Action: "ADOPT_DIRECT_CHANGE", ResourceType: "revision",
			ResourceID: fmt.Sprint(rev), Details: m, ClientIP: "control-plane:" + s.nodeID,
		})
		slog.Info("adopted direct database change as a new revision", "revision", rev, "apis", drift.ChangedAPIs, "plans", drift.ChangedPlans)
		return nil
	})
	if err != nil {
		return drift, 0, err
	}
	return drift, rev, adoptErr
}

func prefixed(p string, names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = p + n
	}
	return out
}

// StartDriftAdopter listens for table change notifications and adopts direct SQL
// edits within milliseconds, with a periodic sweep for missed notifications.
func (s *Server) StartDriftAdopter(ctx context.Context, sweep time.Duration) {
	if s.store == nil {
		return
	}
	trigger := make(chan struct{}, 1)
	poke := func() {
		select {
		case trigger <- struct{}{}:
		default:
		}
	}
	go s.listenTableChanges(ctx, poke)
	poke() // adopt anything changed while the control plane was down
	tick := time.NewTicker(sweep)
	defer tick.Stop()
	lastWarn := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger:
			time.Sleep(30 * time.Millisecond) // coalesce multi-statement edits
		case <-tick.C:
		}
		actx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, _, err := s.adoptConfigDrift(actx)
		cancel()
		if err != nil && err.Error() != lastWarn {
			slog.Warn("config drift adoption", "err", err)
		}
		if err != nil {
			lastWarn = err.Error()
		} else {
			lastWarn = ""
		}
	}
}

func (s *Server) listenTableChanges(ctx context.Context, poke func()) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := func() error {
			conn, err := s.store.Pool.Acquire(ctx)
			if err != nil {
				return err
			}
			defer conn.Release()
			if _, err := conn.Exec(ctx, "LISTEN "+store.ConfigChannel); err != nil {
				return err
			}
			backoff = time.Second
			poke()
			for {
				n, err := conn.Conn().WaitForNotification(ctx)
				if err != nil {
					return err
				}
				var payload struct {
					Table string `json:"table"`
				}
				_ = json.Unmarshal([]byte(n.Payload), &payload)
				if payload.Table == "apis" || payload.Table == "plans" {
					poke()
				}
			}
		}()
		if ctx.Err() != nil {
			return
		}
		slog.Warn("drift listener disconnected; retrying", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// getConfigDrift reports whether the tables differ from the newest revision.
func (s *Server) getConfigDrift(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.DetectConfigDrift(r.Context())
	s.respond(w, http.StatusOK, d, err)
}
