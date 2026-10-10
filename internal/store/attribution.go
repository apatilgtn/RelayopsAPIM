package store

import (
	"bytes"
	"context"
	"encoding/json"
	"time"
)

// RevisionChange says what a revision changed relative to another: the APIs
// it added, removed or modified, and whether it changed something that
// affects every API (plans, or a revision without an API list).
type RevisionChange struct {
	APIs   []string `json:"apis"`
	Global bool     `json:"global"`
}

// ChangedAPIs compares two revisions' configuration.
func (s *Store) ChangedAPIs(ctx context.Context, candidate, baseline int64) (RevisionChange, error) {
	load := func(rev int64) (revisionContent, error) {
		var raw []byte
		err := s.Pool.QueryRow(ctx, `SELECT COALESCE(snapshot_data::text, '')::bytea FROM config_revisions WHERE revision = $1`, rev).Scan(&raw)
		return parseRevisionContent(raw), mapErr(err)
	}
	c, err := load(candidate)
	if err != nil {
		return RevisionChange{}, err
	}
	b, err := load(baseline)
	if err != nil {
		return RevisionChange{}, err
	}
	var ch RevisionChange
	if !c.HasAPIs || !b.HasAPIs {
		ch.Global = true // legacy revision: cannot tell which APIs changed
	}
	if c.HasPlans != b.HasPlans || !samePlans(c.Plans, b.Plans) {
		ch.Global = true
	}
	canon := func(a API) []byte {
		a.CreatedAt, a.UpdatedAt = time.Time{}, time.Time{}
		raw, _ := json.Marshal(a)
		return raw
	}
	before := map[string][]byte{}
	for _, a := range b.APIs {
		before[a.ID] = canon(a)
	}
	seen := map[string]bool{}
	for _, a := range c.APIs {
		seen[a.ID] = true
		if old, ok := before[a.ID]; !ok || !bytes.Equal(old, canon(a)) {
			ch.APIs = append(ch.APIs, a.ID)
		}
	}
	for id := range before {
		if !seen[id] {
			ch.APIs = append(ch.APIs, id)
		}
	}
	return ch, nil
}

func samePlans(a, b map[string]Plan) bool {
	if len(a) != len(b) {
		return false
	}
	for id, p := range a {
		q, ok := b[id]
		if !ok {
			return false
		}
		p.CreatedAt, p.UpdatedAt, q.CreatedAt, q.UpdatedAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
		x, _ := json.Marshal(p)
		y, _ := json.Marshal(q)
		if !bytes.Equal(x, y) {
			return false
		}
	}
	return true
}

// RevisionErrorStatsByAPI is RevisionErrorStatsBetween broken down by API
// (weighted by request-log sampling).
func (s *Store) RevisionErrorStatsByAPI(ctx context.Context, from, to time.Time, revision int64) (map[string]RevisionStats, error) {
	rows, err := s.Pool.Query(ctx, `SELECT COALESCE(api_id::text, ''), round(sum(sample_weight))::bigint,
		round(COALESCE(sum(sample_weight) FILTER (WHERE status >= 500), 0))::bigint
		FROM request_logs
		WHERE ts >= $1 AND ts < $3 AND config_revision = $2
		GROUP BY 1`, from, revision, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RevisionStats{}
	for rows.Next() {
		var id string
		st := RevisionStats{Revision: revision}
		if err := rows.Scan(&id, &st.Requests, &st.Errors); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}
