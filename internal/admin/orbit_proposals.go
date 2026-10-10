package admin

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/relayops/apim/internal/orbit"
	"github.com/relayops/apim/internal/store"
)

// Orbit proposals: Orbit may draft a change to an API's settings, never make
// one. A draft is previewed with the regular change-impact preview and
// returned as a signed token. Applying it is a separate request by a person,
// which runs as that person (their role decides) and refuses if the API
// changed since the draft or the draft has expired.

const proposalTTL = 30 * time.Minute

// proposalFields are the API settings Orbit may propose to change, with
// their console labels and validators.
var proposalFields = map[string]struct {
	label string
	check func(any) (any, error)
}{
	"rate_limit_per_minute": {"Rate limit (requests/min per client)", intIn(0, 1_000_000)},
	"quota_per_day":         {"Daily quota", intIn(0, 1_000_000_000)},
	"quota_per_month":       {"Monthly quota", intIn(0, 1_000_000_000)},
	"timeout_ms":            {"Upstream timeout (ms)", intIn(100, 300_000)},
	"require_approval":      {"Require subscription approval", isBool},
	"enabled":               {"Enabled", isBool},
	"cors_enabled":          {"CORS", isBool},
	"auth_type":             {"Authentication", oneOf("none", "api_key")},
	"visibility":            {"Catalog visibility", oneOf("public", "private", "internal")},
}

func intIn(lo, hi int) func(any) (any, error) {
	return func(v any) (any, error) {
		f, ok := v.(float64)
		if !ok || f != float64(int(f)) || int(f) < lo || int(f) > hi {
			return nil, fmt.Errorf("must be a whole number from %d to %d", lo, hi)
		}
		return int(f), nil
	}
}

func isBool(v any) (any, error) {
	b, ok := v.(bool)
	if !ok {
		return nil, errors.New("must be true or false")
	}
	return b, nil
}

func oneOf(values ...string) func(any) (any, error) {
	return func(v any) (any, error) {
		s, _ := v.(string)
		for _, ok := range values {
			if s == ok {
				return s, nil
			}
		}
		return nil, fmt.Errorf("must be one of %s", strings.Join(values, ", "))
	}
}

// Proposal is what the console shows for a drafted change.
type Proposal struct {
	Token           string           `json:"token"`
	APIID           string           `json:"api_id"`
	APIName         string           `json:"api_name"`
	Reason          string           `json:"reason"`
	Changes         []ProposalChange `json:"changes"`
	Impacts         any              `json:"impacts,omitempty"`
	RecentRequests  any              `json:"recent_requests,omitempty"`
	ActiveConsumers any              `json:"active_consumers,omitempty"`
	ExpiresAt       time.Time        `json:"expires_at"`
}

// ProposalChange is one field's current and proposed value.
type ProposalChange struct {
	Field string `json:"field"`
	Label string `json:"label"`
	From  any    `json:"from"`
	To    any    `json:"to"`
}

// proposalClaims is the signed content of a proposal token.
type proposalClaims struct {
	APIID   string         `json:"api"`
	Set     map[string]any `json:"set"`
	Reason  string         `json:"reason"`
	User    string         `json:"user"`
	Base    time.Time      `json:"base"` // API updated_at when drafted
	Expires int64          `json:"exp"`
}

var (
	proposalKeyOnce sync.Once
	proposalKey     []byte
)

// proposalSigningKey derives from the admin token so every control-plane
// replica accepts proposals drafted by another; without one, a random
// per-process key is used.
func (s *Server) proposalSigningKey() []byte {
	if s.token != "" {
		sum := sha256.Sum256([]byte("relayops-orbit-proposal:" + s.token))
		return sum[:]
	}
	proposalKeyOnce.Do(func() {
		proposalKey = make([]byte, 32)
		_, _ = rand.Read(proposalKey)
	})
	return proposalKey
}

func (s *Server) signProposal(c proposalClaims) string {
	raw, _ := json.Marshal(c)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, s.proposalSigningKey())
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) verifyProposal(token string) (proposalClaims, error) {
	var c proposalClaims
	payload, sig, ok := strings.Cut(token, ".")
	if !ok {
		return c, errors.New("malformed proposal")
	}
	mac := hmac.New(sha256.New, s.proposalSigningKey())
	mac.Write([]byte(payload))
	want := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, want) {
		return c, errors.New("proposal signature is not valid")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || json.Unmarshal(raw, &c) != nil {
		return c, errors.New("malformed proposal")
	}
	if time.Now().Unix() > c.Expires {
		return c, errors.New("this proposal has expired; ask Orbit again")
	}
	return c, nil
}

// proposeTool lets the model draft a change; collect receives each draft so
// the console can show it with the answer.
func (s *Server) proposeTool(r *http.Request, user store.AdminUser, collect func(Proposal)) orbit.Tool {
	fields := make([]string, 0, len(proposalFields))
	for f := range proposalFields {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	return orbit.Tool{
		Name: "propose_api_change",
		Description: "Draft a change to one API's settings for a person to review and apply in the console. Nothing changes until they click Apply. " +
			"Allowed fields: " + strings.Join(fields, ", ") + ". auth_type may only be none or api_key. Use API IDs from list_apis. " +
			"Returns the change and its impact on recent traffic; explain both to the user. Size rate limits from each client's busiest minute, not averages. " +
			"If the impact shows a limit would refuse real traffic, propose again with a higher value or explain the trade-off.",
		Parameters: map[string]any{"type": "object", "required": []string{"api_id", "changes", "reason"}, "properties": map[string]any{
			"api_id":  map[string]any{"type": "string"},
			"changes": map[string]any{"type": "object", "description": "Field to new value, e.g. {\"rate_limit_per_minute\": 600}"},
			"reason":  map[string]any{"type": "string", "description": "One sentence: why this change"},
		}},
		Run: func(ctx context.Context, args map[string]any) (string, error) {
			p, err := s.draftProposal(ctx, r, user, args)
			if err != nil {
				return "", err
			}
			collect(p)
			summary := map[string]any{"drafted": true, "api": p.APIName, "changes": p.Changes, "impacts": p.Impacts,
				"recent_requests": p.RecentRequests, "active_consumers": p.ActiveConsumers,
				"note": "Shown to the user as a card with an Apply button. Do not claim it is applied."}
			out, _ := json.Marshal(summary)
			return string(out), nil
		},
	}
}

func (s *Server) draftProposal(ctx context.Context, r *http.Request, user store.AdminUser, args map[string]any) (Proposal, error) {
	apiID := argString(args, "api_id")
	if !safeIDPattern.MatchString(apiID) {
		return Proposal{}, errors.New("api_id must be an API ID from list_apis")
	}
	reason := argString(args, "reason")
	if len(reason) > 300 {
		reason = reason[:300]
	}
	raw, _ := args["changes"].(map[string]any)
	if len(raw) == 0 {
		return Proposal{}, errors.New("changes must name at least one field")
	}
	// The API as this user sees it (tenant scope applies).
	cur, err := s.orbitGet(ctx, r, "/api/apis/"+url.PathEscape(apiID), nil)
	if err != nil {
		return Proposal{}, err
	}
	var current map[string]any
	_ = json.Unmarshal([]byte(cur), &current)
	set := map[string]any{}
	var changes []ProposalChange
	for field, v := range raw {
		spec, ok := proposalFields[field]
		if !ok {
			return Proposal{}, fmt.Errorf("Orbit cannot propose changes to %q", field)
		}
		val, err := spec.check(v)
		if err != nil {
			return Proposal{}, fmt.Errorf("%s %v", field, err)
		}
		from := current[field]
		if fmt.Sprint(normalizeNumber(from)) == fmt.Sprint(val) {
			continue // already set
		}
		set[field] = val
		changes = append(changes, ProposalChange{Field: field, Label: spec.label, From: normalizeNumber(from), To: val})
	}
	if len(set) == 0 {
		return Proposal{}, errors.New("the API already has these settings; nothing to change")
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Field < changes[j].Field })
	// The same preview the console's "Preview" action runs.
	var preview map[string]any
	if out, err := s.orbitDo(ctx, r, http.MethodPost, "/api/apis/"+url.PathEscape(apiID)+"/preview-change", set, nil); err != nil {
		return Proposal{}, fmt.Errorf("could not preview the change: %v", err)
	} else {
		_ = json.Unmarshal([]byte(out), &preview)
	}
	// The change-impact preview does not simulate rate limits, so check a
	// proposed limit against what each client actually sent per minute.
	impacts, _ := preview["impacts"].([]any)
	if limit, ok := set["rate_limit_per_minute"].(int); ok && limit > 0 && s.store != nil {
		if ex, err := s.store.RateLimitExposure(ctx, apiID, limit, 24*time.Hour); err == nil {
			item := map[string]any{"severity": "info",
				"message":      fmt.Sprintf("No client sent more than %d requests in any minute in the last 24 hours, so %d/min leaves room.", ex.PeakPerClientMinute, limit),
				"metric_value": fmt.Sprintf("busiest client minute: %d requests", ex.PeakPerClientMinute)}
			if ex.WouldRefuse > 0 {
				sev := "warning"
				if ex.WouldRefuse >= 100 || ex.ClientsAffected > 1 {
					sev = "critical"
				}
				item = map[string]any{"severity": sev,
					"message":      fmt.Sprintf("This limit would have refused %d requests from %d client(s) in the last 24 hours: the busiest client sent %d requests in one minute.", ex.WouldRefuse, ex.ClientsAffected, ex.PeakPerClientMinute),
					"metric_value": fmt.Sprintf("a limit of at least %d/min would refuse none", ex.PeakPerClientMinute)}
			}
			impacts = append([]any{item}, impacts...)
			preview["impacts"] = impacts
		}
	}
	base, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(current["updated_at"]))
	exp := time.Now().Add(proposalTTL)
	token := s.signProposal(proposalClaims{APIID: apiID, Set: set, Reason: reason, User: orbitUserKey(user), Base: base, Expires: exp.Unix()})
	name, _ := current["name"].(string)
	return Proposal{Token: token, APIID: apiID, APIName: name, Reason: reason, Changes: changes,
		Impacts: preview["impacts"], RecentRequests: preview["recent_requests"], ActiveConsumers: preview["active_consumers"], ExpiresAt: exp}, nil
}

func normalizeNumber(v any) any {
	if f, ok := v.(float64); ok && f == float64(int(f)) {
		return int(f)
	}
	return v
}

// applyOrbitProposal applies a drafted change as the signed-in user.
func (s *Server) applyOrbitProposal(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &in) {
		return
	}
	c, err := s.verifyProposal(in.Token)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_proposal", err.Error())
		return
	}
	user, _ := r.Context().Value(userCtxKey{}).(store.AdminUser)
	if c.User != orbitUserKey(user) {
		writeErr(w, http.StatusForbidden, "proposal_not_yours", "this proposal was drafted for another user; ask Orbit yourself")
		return
	}
	ctx := r.Context()
	cur, err := s.orbitGet(ctx, r, "/api/apis/"+url.PathEscape(c.APIID), nil)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	var current struct {
		Name      string    `json:"name"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	_ = json.Unmarshal([]byte(cur), &current)
	if !current.UpdatedAt.Equal(c.Base) {
		writeErr(w, http.StatusConflict, "proposal_stale", "the API changed after Orbit drafted this; ask Orbit again so the proposal reflects the current settings")
		return
	}
	// The regular update path, as this user: their role decides.
	if _, err := s.orbitDo(ctx, r, http.MethodPut, "/api/apis/"+url.PathEscape(c.APIID), c.Set, nil); err != nil {
		status := http.StatusBadGateway
		if strings.HasPrefix(err.Error(), "HTTP 403") {
			status = http.StatusForbidden
		} else if strings.HasPrefix(err.Error(), "HTTP 4") {
			status = http.StatusBadRequest
		}
		writeErr(w, status, "apply_failed", err.Error())
		return
	}
	s.orbitStats.applied.Add(1)
	s.audit(r, "ORBIT_PROPOSAL_APPLIED", "api", c.APIID, map[string]any{"api": current.Name, "changes": c.Set, "reason": c.Reason})
	writeJSON(w, http.StatusOK, map[string]any{"applied": true, "api_id": c.APIID, "api_name": current.Name, "changes": c.Set})
}
