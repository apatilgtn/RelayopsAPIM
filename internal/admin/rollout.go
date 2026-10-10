package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/relayops/apim/internal/alerts"
	"github.com/relayops/apim/internal/coordinator"
	"github.com/relayops/apim/internal/store"
)

// autoRollbackLockKey is the Postgres advisory lock that elects the single
// control-plane node allowed to evaluate (and act on) auto-rollback at a time.
const autoRollbackLockKey int64 = 0x52454c41594f5053 // "RELAYOPS"

// ---------------------------------------------------------------------------
// Coordinated, durable automatic rollback
// ---------------------------------------------------------------------------

// AutoRollbackConfig is the API shape of the auto-rollback settings. It embeds the
// persisted settings and adds the request-only reset flag and live rollout state.
type AutoRollbackConfig struct {
	store.AutoRollbackSettings
	ResetCooldown           bool  `json:"reset_cooldown,omitempty"`
	CurrentActiveRevision   int64 `json:"current_active_revision,omitempty"`
	CurrentBaselineRevision int64 `json:"current_baseline_revision,omitempty"`
	CurrentCanaryRevision   int64 `json:"current_canary_revision,omitempty"`
}

func (s *Server) autoRollbackView(ctx context.Context) (AutoRollbackConfig, error) {
	cfg, err := s.store.GetAutoRollbackSettings(ctx)
	if err != nil {
		return AutoRollbackConfig{}, err
	}
	if cfg.CooldownUntil != nil && time.Now().After(*cfg.CooldownUntil) {
		cfg.CooldownUntil = nil
	}
	view := AutoRollbackConfig{AutoRollbackSettings: cfg}
	if stable, canary, err := s.store.GetRolloutState(ctx); err == nil {
		view.CurrentCanaryRevision = canary
		if canary > 0 {
			view.CurrentActiveRevision, view.CurrentBaselineRevision = canary, stable
		} else {
			view.CurrentActiveRevision = stable
			view.CurrentBaselineRevision, _ = s.store.PreviousActiveRevision(ctx, stable)
		}
	}
	return view, nil
}

func (s *Server) getAutoRollbackConfig(w http.ResponseWriter, r *http.Request) {
	view, err := s.autoRollbackView(r.Context())
	s.respond(w, http.StatusOK, view, err)
}

func (s *Server) setAutoRollbackConfig(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled                    *bool    `json:"enabled"`
		ErrorRateThresholdPercent  *float64 `json:"error_rate_threshold_percent"`
		EvaluationWindowSeconds    *int     `json:"evaluation_window_seconds"`
		MinRequests                *int     `json:"min_requests"`
		CooldownSeconds            *int     `json:"cooldown_seconds"`
		MinBaselineRequests        *int     `json:"min_baseline_requests"`
		InsufficientBaselineAction *string  `json:"insufficient_baseline_action"`
		ResetCooldown              bool     `json:"reset_cooldown"`
	}
	if !decode(w, r, &in) {
		return
	}
	cur, err := s.store.GetAutoRollbackSettings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	before := cur
	if in.Enabled != nil {
		cur.Enabled = *in.Enabled
	}
	if in.ErrorRateThresholdPercent != nil {
		cur.ErrorRateThresholdPercent = *in.ErrorRateThresholdPercent
	}
	if in.EvaluationWindowSeconds != nil {
		cur.EvaluationWindowSeconds = *in.EvaluationWindowSeconds
	}
	if in.MinRequests != nil {
		cur.MinRequests = *in.MinRequests
	}
	if in.CooldownSeconds != nil {
		cur.CooldownSeconds = *in.CooldownSeconds
	}
	if in.MinBaselineRequests != nil {
		cur.MinBaselineRequests = *in.MinBaselineRequests
	}
	if in.InsufficientBaselineAction != nil {
		cur.InsufficientBaselineAction = strings.TrimSpace(*in.InsufficientBaselineAction)
	}
	if err := validateAutoRollback(cur); err != nil {
		writeErr(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	saved, err := s.store.UpdateAutoRollbackSettings(r.Context(), cur, in.ResetCooldown, s.getActor(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditWithState(r, "UPDATE_AUTO_ROLLBACK_CONFIG", "system", "auto_rollback", map[string]any{
		"enabled": saved.Enabled, "threshold": saved.ErrorRateThresholdPercent, "window_sec": saved.EvaluationWindowSeconds,
		"min_requests": saved.MinRequests, "cooldown_seconds": saved.CooldownSeconds, "reset_cooldown": in.ResetCooldown,
		"min_baseline_requests": saved.MinBaselineRequests, "insufficient_baseline_action": saved.InsufficientBaselineAction,
	}, toMap(before), toMap(saved), nil)
	view, err := s.autoRollbackView(r.Context())
	s.respond(w, http.StatusOK, view, err)
}

func validateAutoRollback(c store.AutoRollbackSettings) error {
	switch {
	case c.ErrorRateThresholdPercent <= 0 || c.ErrorRateThresholdPercent > 100:
		return errors.New("error_rate_threshold_percent must be greater than 0 and at most 100")
	case c.EvaluationWindowSeconds < 10 || c.EvaluationWindowSeconds > 86400:
		return errors.New("evaluation_window_seconds must be between 10 and 86400")
	case c.MinRequests < 1:
		return errors.New("min_requests must be at least 1")
	case c.CooldownSeconds < 0:
		return errors.New("cooldown_seconds must be >= 0")
	case c.MinBaselineRequests < 0:
		return errors.New("min_baseline_requests must be >= 0")
	case c.InsufficientBaselineAction != "absolute" && c.InsufficientBaselineAction != "hold":
		return errors.New("insufficient_baseline_action must be 'absolute' or 'hold'")
	}
	return nil
}

func toMap(v any) map[string]any {
	raw, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

type AutoRollbackEvaluationResult struct {
	Evaluated            bool    `json:"evaluated"`
	Triggered            bool    `json:"triggered"`
	Skipped              bool    `json:"skipped,omitempty"` // another control-plane node holds the evaluation lock
	Mode                 string  `json:"mode,omitempty"`    // "canary" or "release"
	Reason               string  `json:"reason"`
	ActiveRevision       int64   `json:"active_revision"`
	BaselineRevision     int64   `json:"baseline_revision"`
	RestoredRevision     int64   `json:"restored_revision,omitempty"`
	ActiveSampleCount    int     `json:"active_sample_count"`
	ActiveErrorCount     int     `json:"active_error_count"`
	ActiveErrorRatePct   float64 `json:"active_error_rate_pct"`
	BaselineSampleCount  int     `json:"baseline_sample_count"`
	BaselineErrorCount   int     `json:"baseline_error_count"`
	BaselineErrorRatePct float64 `json:"baseline_error_rate_pct"`
	ThresholdPct         float64 `json:"threshold_pct"`
	InCooldown           bool    `json:"in_cooldown"`
	EvaluatedBy          string  `json:"evaluated_by,omitempty"`

	BaselineSufficient  bool      `json:"baseline_sufficient"`
	DecisionBasis       string    `json:"decision_basis,omitempty"` // relative | absolute | held | not_attributed
	// Attribution: what the candidate changed and how those APIs fared.
	ChangedAPIs              []string `json:"changed_apis,omitempty"`
	ChangedAPIErrorRatePct   float64  `json:"changed_api_error_rate_pct,omitempty"`
	UnchangedAPIErrorRatePct float64  `json:"unchanged_api_error_rate_pct,omitempty"`
	BaselineWindowStart time.Time `json:"baseline_window_start,omitempty"`
	BaselineWindowEnd   time.Time `json:"baseline_window_end,omitempty"`
}

// evaluateAutoRollback runs one evaluation if this node wins the cluster lock.
func (s *Server) evaluateAutoRollback(ctx context.Context) AutoRollbackEvaluationResult {
	if s.store == nil {
		return AutoRollbackEvaluationResult{Reason: "store uninitialized"}
	}
	var res AutoRollbackEvaluationResult
	acquired, err := s.store.WithAdvisoryLock(ctx, autoRollbackLockKey, func(ctx context.Context) error {
		res = s.evaluateAutoRollbackLocked(ctx)
		return nil
	})
	if err != nil {
		return AutoRollbackEvaluationResult{Reason: fmt.Sprintf("evaluation failed: %v", err)}
	}
	if !acquired {
		return AutoRollbackEvaluationResult{Skipped: true, Reason: "another control-plane node is evaluating auto-rollback; skipped"}
	}
	return res
}

func (s *Server) evaluateAutoRollbackLocked(ctx context.Context) AutoRollbackEvaluationResult {
	res := AutoRollbackEvaluationResult{EvaluatedBy: s.nodeID}
	cfg, err := s.store.GetAutoRollbackSettings(ctx)
	if err != nil {
		res.Reason = fmt.Sprintf("failed to load auto-rollback settings: %v", err)
		return res
	}
	_ = s.store.MarkAutoRollbackEvaluated(ctx, s.nodeID)
	res.ThresholdPct = cfg.ErrorRateThresholdPercent

	if !cfg.Enabled {
		res.Reason = "auto-rollback supervisor is disabled"
		return res
	}
	if cfg.CooldownUntil != nil && time.Now().Before(*cfg.CooldownUntil) {
		res.InCooldown = true
		res.Reason = fmt.Sprintf("in cooldown period until %s", cfg.CooldownUntil.Format(time.RFC3339))
		return res
	}

	stable, canary, err := s.store.GetRolloutState(ctx)
	if err != nil {
		res.Reason = fmt.Sprintf("failed to read rollout state: %v", err)
		return res
	}
	var candidate, baseline int64
	if canary > 0 {
		res.Mode = "canary"
		candidate, baseline = canary, stable
	} else {
		res.Mode = "release"
		candidate = stable
		rev, err := s.store.GetConfigRevisionMeta(ctx, candidate)
		if err != nil {
			res.Reason = "no configuration revisions found"
			return res
		}
		if rev.RollbackOf != nil {
			res.ActiveRevision = candidate
			res.Reason = fmt.Sprintf("rev_%d is already a rollback of rev_%d", candidate, *rev.RollbackOf)
			return res
		}
		baseline, _ = s.store.PreviousActiveRevision(ctx, candidate)
	}
	res.ActiveRevision, res.BaselineRevision = candidate, baseline
	if baseline <= 0 {
		res.Reason = "no baseline revision available to rollback to"
		return res
	}

	// Fleet-wide request outcomes from every gateway node's logs.
	//  - canary:  both revisions serve concurrently, so compare the same recent window.
	//  - release: the baseline stopped serving when the candidate was published, so its
	//             window is the evaluation window immediately before publication.
	window := time.Duration(cfg.EvaluationWindowSeconds) * time.Second
	now := time.Now()
	candFrom, candTo := now.Add(-window), now.Add(time.Minute)
	baseFrom, baseTo := candFrom, candTo
	if res.Mode == "release" {
		if rev, err := s.store.GetConfigRevisionMeta(ctx, candidate); err == nil {
			baseFrom, baseTo = rev.CreatedAt.Add(-window), rev.CreatedAt
			if candFrom.Before(rev.CreatedAt) {
				candFrom = rev.CreatedAt
			}
		}
	}
	res.BaselineWindowStart, res.BaselineWindowEnd = baseFrom, baseTo
	cstats, err := s.store.RevisionErrorStatsBetween(ctx, candFrom, candTo, candidate)
	if err != nil {
		res.Reason = fmt.Sprintf("failed to aggregate request logs: %v", err)
		return res
	}
	bstats, err := s.store.RevisionErrorStatsBetween(ctx, baseFrom, baseTo, baseline)
	if err != nil {
		res.Reason = fmt.Sprintf("failed to aggregate request logs: %v", err)
		return res
	}
	cs, bs := cstats[candidate], bstats[baseline]
	res.ActiveSampleCount, res.ActiveErrorCount = int(cs.Requests), int(cs.Errors)
	res.BaselineSampleCount, res.BaselineErrorCount = int(bs.Requests), int(bs.Errors)
	res.Evaluated = true

	if res.ActiveSampleCount < cfg.MinRequests {
		res.Reason = fmt.Sprintf("sample size %d on rev_%d below minimum traffic threshold %d", res.ActiveSampleCount, candidate, cfg.MinRequests)
		return res
	}
	res.ActiveErrorRatePct = pct(cs.Errors, cs.Requests)
	res.BaselineErrorRatePct = pct(bs.Errors, bs.Requests)
	res.BaselineSufficient = res.BaselineSampleCount >= cfg.MinBaselineRequests

	breach, basis, reason := decideRollback(cfg, candidate, baseline, res)
	res.DecisionBasis = basis
	if !breach {
		res.Reason = reason
		return res
	}
	// Attribution: roll back only when the errors are on what the candidate
	// changed. An upstream outage that also hits APIs the revision did not
	// touch is not the release's fault, and rolling back would not fix it.
	if ok, why := s.attributeBreach(ctx, cfg, candidate, baseline, candFrom, candTo, &res); !ok {
		res.DecisionBasis = "not_attributed"
		res.Reason = fmt.Sprintf("not rolled back: %s %s", reason, why)
		s.alertHeldRollback(candidate, res.Reason)
		return res
	}

	// Breach: withdraw the candidate and restore the baseline.
	const actor = "auto_rollback_supervisor"
	var restored int64
	if res.Mode == "canary" {
		restored, err = s.store.AbortCanary(ctx, candidate, actor)
	} else {
		restored, err = s.store.RollbackConfigRevision(ctx, baseline, actor)
		if err == nil {
			_ = s.store.MarkRolledBack(ctx, candidate)
		}
	}
	if err != nil {
		res.Reason = fmt.Sprintf("rollback execution failed: %v", err)
		return res
	}
	res.RestoredRevision = restored
	res.Triggered = true
	res.Reason = fmt.Sprintf("Automatic rollback triggered (%s, %s): %s Restored rev_%d as rev_%d.",
		res.Mode, res.DecisionBasis, reason, baseline, restored)
	_ = s.store.RecordAutoRollbackTrigger(ctx, candidate, res.Reason, time.Duration(cfg.CooldownSeconds)*time.Second)
	_ = s.store.CreateRichAuditLog(ctx, store.AuditLog{
		Actor: actor, ActorRole: "system", Action: "AUTO_ROLLBACK", ResourceType: "revision",
		ResourceID: strconv.FormatInt(candidate, 10),
		Details:    toMap(res),
		ClientIP:   "control-plane:" + s.nodeID,
	})
	slog.Warn("automatic rollback executed", "mode", res.Mode, "bad_revision", candidate, "baseline", baseline, "new_revision", restored,
		"error_rate_pct", res.ActiveErrorRatePct, "node", s.nodeID)
	if s.hub != nil {
		s.hub.Publish("rollback", map[string]any{
			"new_revision": restored, "rolled_back_from": candidate, "restored_to": baseline,
			"mode": res.Mode, "reason": res.Reason, "at": time.Now(),
		})
	}
	return res
}

// decideRollback applies the threshold to an evaluated sample. It is explicit
// about the evidence used:
//
//	relative: baseline sample is sufficient; act when the candidate breaches the
//	          threshold AND is more than 1 point worse than the baseline.
//	absolute: baseline sample is insufficient and the policy is "absolute"; act on
//	          the threshold alone (no comparative evidence; a shared upstream outage
//	          can also trigger this).
//	held:     baseline sample is insufficient and the policy is "hold"; never act.
func decideRollback(cfg store.AutoRollbackSettings, candidate, baseline int64, res AutoRollbackEvaluationResult) (bool, string, string) {
	over := res.ActiveErrorRatePct >= cfg.ErrorRateThresholdPercent
	cand := fmt.Sprintf("rev_%d had %.1f%% errors over %d requests", candidate, res.ActiveErrorRatePct, res.ActiveSampleCount)
	if res.BaselineSufficient {
		base := fmt.Sprintf("baseline rev_%d %.1f%% over %d requests", baseline, res.BaselineErrorRatePct, res.BaselineSampleCount)
		if over && res.ActiveErrorRatePct > res.BaselineErrorRatePct+1.0 {
			return true, "relative", fmt.Sprintf("%s vs %s (threshold %.1f%%).", cand, base, cfg.ErrorRateThresholdPercent)
		}
		return false, "relative", fmt.Sprintf("healthy: %s, %s, threshold %.1f%%", cand, base, cfg.ErrorRateThresholdPercent)
	}
	thin := fmt.Sprintf("baseline rev_%d has only %d requests (minimum %d for a comparison)", baseline, res.BaselineSampleCount, cfg.MinBaselineRequests)
	if cfg.InsufficientBaselineAction == "hold" {
		if over {
			return false, "held", fmt.Sprintf("holding: %s and breaches the %.1f%% threshold, but %s; policy is hold", cand, cfg.ErrorRateThresholdPercent, thin)
		}
		return false, "held", fmt.Sprintf("healthy: %s; %s", cand, thin)
	}
	if over {
		return true, "absolute", fmt.Sprintf("%s, at or above the %.1f%% threshold; %s, so the absolute threshold was applied.", cand, cfg.ErrorRateThresholdPercent, thin)
	}
	return false, "absolute", fmt.Sprintf("healthy: %s, below the %.1f%% threshold; %s", cand, cfg.ErrorRateThresholdPercent, thin)
}

// attributeBreach decides whether a breach can be blamed on the candidate.
// It returns false (with the reason) when the APIs the candidate changed are
// healthy, or too quiet to judge, while unchanged APIs carry the errors.
// When the change cannot be determined it keeps the previous behaviour.
// sharedFailureMarginPct is how much worse (in percentage points) the changed
// APIs may fail than unchanged ones before the release is blamed anyway.
const sharedFailureMarginPct = 10.0

func (s *Server) attributeBreach(ctx context.Context, cfg store.AutoRollbackSettings, candidate, baseline int64, from, to time.Time, res *AutoRollbackEvaluationResult) (bool, string) {
	// Revisions are immutable, so the comparison is computed once per pair
	// (it reads two full snapshots; evaluations repeat every few seconds).
	key := [2]int64{candidate, baseline}
	change, cached := s.changeCache.Load(key)
	if !cached {
		c, err := s.store.ChangedAPIs(ctx, candidate, baseline)
		if err != nil {
			return true, ""
		}
		s.changeCache.Store(key, c)
		change = c
	}
	if change.Global || len(change.APIs) == 0 {
		return true, ""
	}
	byAPI, err := s.store.RevisionErrorStatsByAPI(ctx, from, to, candidate)
	if err != nil {
		return true, ""
	}
	changed := map[string]bool{}
	for _, id := range change.APIs {
		changed[id] = true
	}
	var cReq, cErr, oReq, oErr int64
	for id, st := range byAPI {
		if id == "" {
			continue // not routed to an API: cannot be attributed either way
		}
		if changed[id] {
			cReq, cErr = cReq+st.Requests, cErr+st.Errors
		} else {
			oReq, oErr = oReq+st.Requests, oErr+st.Errors
		}
	}
	if cReq+oReq == 0 {
		return true, ""
	}
	res.ChangedAPIs = change.APIs
	res.ChangedAPIErrorRatePct, res.UnchangedAPIErrorRatePct = pct(cErr, cReq), pct(oErr, oReq)
	switch {
	case cReq < int64(cfg.MinRequests) && oErr > 0:
		return false, fmt.Sprintf("The %d API(s) rev_%d changed served only %d requests, too few to blame them, while unchanged APIs had %.1f%% errors; this looks like an upstream or external problem.",
			len(change.APIs), candidate, cReq, res.UnchangedAPIErrorRatePct)
	case cReq >= int64(cfg.MinRequests) && res.ChangedAPIErrorRatePct < cfg.ErrorRateThresholdPercent:
		return false, fmt.Sprintf("The %d API(s) rev_%d changed are healthy (%.1f%% errors over %d requests); the errors are on APIs it did not change (%.1f%%), so they are not caused by this release.",
			len(change.APIs), candidate, res.ChangedAPIErrorRatePct, cReq, res.UnchangedAPIErrorRatePct)
	case oReq >= int64(cfg.MinRequests) && res.UnchangedAPIErrorRatePct >= cfg.ErrorRateThresholdPercent &&
		res.ChangedAPIErrorRatePct <= res.UnchangedAPIErrorRatePct+sharedFailureMarginPct:
		// APIs the release did not touch fail just as often: a shared
		// upstream, network or dependency outage, which rolling back cannot fix.
		return false, fmt.Sprintf("APIs rev_%d did not change fail as often (%.1f%% errors over %d requests) as the %d it changed (%.1f%%); this looks like a shared upstream or network outage, not the release.",
			candidate, res.UnchangedAPIErrorRatePct, oReq, len(change.APIs), res.ChangedAPIErrorRatePct)
	}
	return true, ""
}

// alertHeldRollback raises one alert per candidate revision whose error
// spike was not attributed to it.
func (s *Server) alertHeldRollback(candidate int64, reason string) {
	if s.heldRollbackRev.Swap(candidate) == candidate {
		return
	}
	s.alerts.Notify(alerts.Alert{Type: "rollback_not_attributed", Severity: "warning",
		Title: fmt.Sprintf("Error spike after rev_%d, not caused by the release", candidate), Text: reason,
		Link: consoleLink("/#/fleet")})
	_ = s.store.CreateRichAuditLog(context.Background(), store.AuditLog{Actor: "auto_rollback_supervisor", ActorRole: "system",
		Action: "AUTO_ROLLBACK_HELD", ResourceType: "revision", ResourceID: strconv.FormatInt(candidate, 10),
		Details: map[string]any{"reason": reason}, ClientIP: "control-plane:" + s.nodeID})
}

func pct(n, d int64) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d) * 100
}

func (s *Server) manualEvaluateAutoRollback(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.evaluateAutoRollback(r.Context()))
}

// DefaultAutoRollbackInterval is how often the supervisor evaluates unless
// WithAutoRollbackInterval says otherwise.
const DefaultAutoRollbackInterval = 3 * time.Second

// WithAutoRollbackInterval sets the supervisor's evaluation interval. Each
// evaluation is about a dozen small queries, so on a metered hosted database
// a longer interval (for example 15s) trades detection delay for egress.
func WithAutoRollbackInterval(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.autoRollbackInterval = d
		}
	}
}

// WithLeaderElector makes only the replica holding a leader lease (for
// example a Kubernetes Lease) run the supervisor, so the other replicas send
// no supervisor queries to the database at all. The advisory lock still
// guards each decision.
func WithLeaderElector(e coordinator.LeaderElector) Option {
	return func(s *Server) { s.elector = e }
}

// supervisorLeaseName names the lease the supervisor replicas compete for.
const supervisorLeaseName = "auto-rollback-supervisor"

// StartAutoRollbackSupervisor evaluates every few seconds. Every control-plane
// node runs it; the advisory lock and the persisted cooldown ensure one decision.
// With a leader elector, non-leaders skip evaluation entirely.
func (s *Server) StartAutoRollbackSupervisor(ctx context.Context) {
	interval := s.autoRollbackInterval
	if interval <= 0 {
		interval = DefaultAutoRollbackInterval
	}
	// The lease outlives a few missed renewals but not a dead leader for long.
	leaseTTL := max(3*interval, 15*time.Second)
	var release func()
	leader := false
	defer func() {
		if release != nil {
			release()
		}
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.store == nil {
				continue
			}
			if s.elector != nil {
				lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				acquired, rel, err := s.elector.TryAcquire(lctx, supervisorLeaseName, leaseTTL)
				cancel()
				switch {
				case err != nil:
					// The lease service is unreachable: fall back to the advisory
					// lock alone rather than leave releases unsupervised.
					slog.Warn("leader election unavailable; evaluating under the database lock", "elector", s.elector.Name(), "err", err)
				case !acquired:
					if leader {
						slog.Info("auto-rollback supervisor lease lost", "elector", s.elector.Name())
					}
					leader, release = false, nil
					continue
				default:
					if !leader {
						slog.Info("auto-rollback supervisor lease acquired", "elector", s.elector.Name(), "node", s.nodeID)
					}
					leader, release = true, rel
				}
			}
			ectx, cancel := context.WithTimeout(ctx, 20*time.Second)
			_ = s.evaluateAutoRollback(ectx)
			cancel()
		}
	}
}

// ---------------------------------------------------------------------------
// Canary deployment: node-group, percentage and header splits
// ---------------------------------------------------------------------------

var headerNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

func validateSplit(sp *store.CanarySplit) error {
	sp.Header = strings.TrimSpace(sp.Header)
	if sp.TrafficPercent < 0 || sp.TrafficPercent > 100 {
		return errors.New("traffic_percent must be between 0 and 100")
	}
	if sp.Header != "" && !headerNamePattern.MatchString(sp.Header) {
		return errors.New("header must be a valid HTTP header name")
	}
	if sp.Header == "" && sp.HeaderValue != "" {
		return errors.New("header_value requires header")
	}
	if len(sp.HeaderValue) > 256 {
		return errors.New("header_value must be at most 256 characters")
	}
	return nil
}

// decodeOptional decodes a JSON body when one was sent.
func decodeOptional(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil || r.ContentLength == 0 {
		return true
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

func (s *Server) deployCanary(w http.ResponseWriter, r *http.Request) {
	revID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_revision", "revision ID must be an integer")
		return
	}
	var split store.CanarySplit
	if !decodeOptional(w, r, &split) {
		return
	}
	if err := validateSplit(&split); err != nil {
		writeErr(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	if err := s.store.DeployCanary(r.Context(), revID, split); err != nil {
		s.fail(w, err)
		return
	}
	mode := "node_group"
	msg := fmt.Sprintf("Revision rev_%d deployed to canary gateway nodes.", revID)
	if split.TrafficPercent > 0 || split.Header != "" {
		mode = "traffic_split"
		msg = fmt.Sprintf("Revision rev_%d serving %d%% of traffic on every node", revID, split.TrafficPercent)
		if split.Header != "" {
			msg += fmt.Sprintf(", plus requests with header %s", split.Header)
		}
		msg += ", and 100% on canary nodes."
	}
	s.audit(r, "DEPLOY_CANARY", "revision", strconv.FormatInt(revID, 10), map[string]any{
		"target_group": "canary", "mode": mode, "traffic_percent": split.TrafficPercent, "header": split.Header,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"revision": revID, "status": "canary", "target_group": "canary", "mode": mode,
		"traffic_percent": split.TrafficPercent, "header": split.Header, "header_value": split.HeaderValue,
		"message": msg,
	})
}

func (s *Server) promoteRevision(w http.ResponseWriter, r *http.Request) {
	revID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_revision", "revision ID must be an integer")
		return
	}

	// Check for superadmin override reason
	overrideReason := r.URL.Query().Get("override_reason")
	if overrideReason == "" {
		overrideReason = r.Header.Get("X-RelayOps-Override-Reason")
	}
	if overrideReason == "" && r.Body != nil && r.ContentLength > 0 {
		var req struct {
			OverrideReason string `json:"override_reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		overrideReason = req.OverrideReason
	}

	u, _ := r.Context().Value(userCtxKey{}).(store.AdminUser)
	isSuperadmin := u.Role == "superadmin"

	reasons, err := s.store.PromoteCanaryWithTestGates(r.Context(), revID, isSuperadmin && strings.TrimSpace(overrideReason) != "")
	if err != nil {
		var blocked *store.GateRejectedError
		if errors.As(err, &blocked) {
			writeErr(w, http.StatusPreconditionFailed, "promotion_gate_failed", blocked.Error())
		} else {
			s.fail(w, err)
		}
		return
	}
	if len(reasons) > 0 {
		s.audit(r, "GATE_OVERRIDE", "revision", strconv.FormatInt(revID, 10), map[string]any{"override_reason": overrideReason, "reasons": reasons})
	}
	s.audit(r, "PROMOTE_REVISION", "revision", strconv.FormatInt(revID, 10), map[string]any{"target_group": "all"})
	writeJSON(w, http.StatusOK, map[string]any{
		"revision": revID, "status": "active", "target_group": "all",
		"message": fmt.Sprintf("Revision rev_%d successfully promoted to 100%% of fleet gateways.", revID),
	})
}

func (s *Server) abortCanary(w http.ResponseWriter, r *http.Request) {
	revID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_revision", "revision ID must be an integer")
		return
	}
	restored, err := s.store.AbortCanary(r.Context(), revID, s.getActor(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "ABORT_CANARY", "revision", strconv.FormatInt(revID, 10), map[string]any{"restored_revision": restored})
	writeJSON(w, http.StatusOK, map[string]any{
		"revision": revID, "status": "rolled_back", "new_revision": restored,
		"message": fmt.Sprintf("Canary rev_%d withdrawn from all nodes; configuration restored as rev_%d.", revID, restored),
	})
}

func (s *Server) getCanaryStatus(w http.ResponseWriter, r *http.Request) {
	revID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_revision", "revision ID must be an integer")
		return
	}
	rev, err := s.store.GetConfigRevisionMeta(r.Context(), revID)
	if err != nil {
		s.fail(w, err)
		return
	}
	fleet, err := s.store.GetFleetStatus(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	split, _ := s.store.GetCanarySplit(r.Context(), revID)

	canaryNodes := []store.NodeAcknowledgement{}
	standardNodes := []store.NodeAcknowledgement{}
	canaryAcked, standardAcked, standardServing := 0, 0, 0
	for _, n := range fleet.Nodes {
		if n.IsCanary || n.NodeGroup == "canary" {
			canaryNodes = append(canaryNodes, n)
			if n.Revision == revID {
				canaryAcked++
			}
		} else {
			standardNodes = append(standardNodes, n)
			if n.Revision == revID {
				standardAcked++
			}
			if n.CanaryRev == revID {
				standardServing++
			}
		}
	}
	canaryConverged := len(canaryNodes) > 0 && canaryAcked == len(canaryNodes)
	fleetConverged := len(fleet.Nodes) > 0 && (canaryAcked+standardAcked) == len(fleet.Nodes)

	var stats map[string]any
	if stable, _, err := s.store.GetRolloutState(r.Context()); err == nil {
		cfg, _ := s.store.GetAutoRollbackSettings(r.Context())
		window := time.Duration(cfg.EvaluationWindowSeconds) * time.Second
		if window <= 0 {
			window = time.Minute
		}
		if st, err := s.store.RevisionErrorStats(r.Context(), window, revID, stable); err == nil {
			stats = map[string]any{
				"window_seconds": int(window.Seconds()),
				"canary":         map[string]any{"revision": revID, "requests": st[revID].Requests, "errors": st[revID].Errors, "error_rate_pct": pct(st[revID].Errors, st[revID].Requests)},
				"baseline":       map[string]any{"revision": stable, "requests": st[stable].Requests, "errors": st[stable].Errors, "error_rate_pct": pct(st[stable].Errors, st[stable].Requests)},
			}
		}
	}

	gateEnforced := false
	gateEligible := true
	var gateReasons []string
	if s.dbAvailable() {
		rows, err := s.store.Pool.Query(r.Context(), `SELECT tenant_id, api_id, required_suite_ids, freshness_seconds FROM test_gate_policies WHERE enforcement_enabled = true`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var tID, aID string
				var reqSuitesJSON []byte
				var freshnessSeconds int
				if err := rows.Scan(&tID, &aID, &reqSuitesJSON, &freshnessSeconds); err == nil {
					gateEnforced = true
					var reqSuites []string
					_ = json.Unmarshal(reqSuitesJSON, &reqSuites)
					if len(reqSuites) > 0 {
						for _, reqSuiteID := range reqSuites {
							var evID string
							var verifiedAt time.Time
							qErr := s.store.Pool.QueryRow(r.Context(), `
								SELECT e.id, e.verified_at
								FROM test_gate_evidence e
								JOIN test_runs r ON r.id = e.run_id
								WHERE e.tenant_id = $1 AND e.api_id = $2 AND e.revision = $3
								  AND e.eligible = true AND r.suite_id = $4
								ORDER BY e.verified_at DESC LIMIT 1`,
								tID, aID, revID, reqSuiteID).Scan(&evID, &verifiedAt)
							if qErr != nil {
								gateEligible = false
								gateReasons = append(gateReasons, fmt.Sprintf("Missing passing evidence for required suite '%s' on API %s", reqSuiteID, aID))
							} else if freshnessSeconds > 0 && time.Since(verifiedAt) > time.Duration(freshnessSeconds)*time.Second {
								gateEligible = false
								gateReasons = append(gateReasons, fmt.Sprintf("Evidence expired for suite '%s' on API %s (verified %s ago)", reqSuiteID, aID, time.Since(verifiedAt).Round(time.Second)))
							}
						}
					} else {
						ev, _ := s.store.GetLatestEligibleEvidence(r.Context(), tID, aID, revID)
						if ev == nil || !ev.Eligible {
							gateEligible = false
							gateReasons = append(gateReasons, fmt.Sprintf("Missing or ineligible test evidence for API %s", aID))
						} else if freshnessSeconds > 0 && time.Since(ev.VerifiedAt) > time.Duration(freshnessSeconds)*time.Second {
							gateEligible = false
							gateReasons = append(gateReasons, fmt.Sprintf("Test evidence for API %s expired (verified %s ago)", aID, time.Since(ev.VerifiedAt).Round(time.Second)))
						}
					}
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"revision":                     revID,
		"status":                       rev.Status,
		"target_group":                 rev.TargetGroup,
		"traffic_percent":              split.TrafficPercent,
		"header":                       split.Header,
		"header_value":                 split.HeaderValue,
		"canary_nodes_count":           len(canaryNodes),
		"canary_acknowledged_count":    canaryAcked,
		"canary_converged":             canaryConverged,
		"standard_nodes_count":         len(standardNodes),
		"standard_acknowledged_count":  standardAcked,
		"standard_nodes_serving_split": standardServing,
		"fleet_converged":              fleetConverged,
		"canary_nodes":                 canaryNodes,
		"standard_nodes":               standardNodes,
		"comparison":                   stats,
		"gate_status": map[string]any{
			"enforced": gateEnforced,
			"eligible": gateEligible,
			"reasons":  gateReasons,
		},
	})
}

// upstreamHealth reports load-balancing, circuit and health-check state on this node.
func (s *Server) upstreamHealth(w http.ResponseWriter, r *http.Request) {
	status := s.gw.UpstreamStatus()
	if status == nil {
		status = []gatewayTargetStatus{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"node_id": s.nodeID, "targets": status})
}
