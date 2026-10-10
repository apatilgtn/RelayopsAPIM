package store

import (
	"context"
	"encoding/json"
	"time"
)

// RefusalCount groups policy refusals by protocol, reason and API.
type RefusalCount struct {
	Protocol string    `json:"protocol"` // http, graphql, grpc, mcp
	Reason   string    `json:"reason"`
	APIName  string    `json:"api_name"`
	Count    float64   `json:"count"` // weighted by sampling
	Plugin   bool      `json:"wasm"`  // a WASM plugin ran on these requests
	LastSeen time.Time `json:"last_seen"`
}

// Refusal is one refused request.
type Refusal struct {
	TS           time.Time      `json:"ts"`
	Protocol     string         `json:"protocol"`
	APIName      string         `json:"api_name"`
	ConsumerName string         `json:"consumer_name"`
	Method       string         `json:"method"`
	Path         string         `json:"path"`
	Status       int            `json:"status"`
	Reason       string         `json:"reason"`
	Route        string         `json:"matched_route"`
	Error        string         `json:"error"`
	Evaluations  map[string]any `json:"policy_evaluations"`
	RequestID    string         `json:"request_id"`
}

// refusalProtocol classifies a log row by the protocol inspector that ran.
const refusalProtocol = `CASE WHEN policy_evaluations ? 'mcp' THEN 'mcp'
	WHEN policy_evaluations ? 'graphql' THEN 'graphql'
	WHEN policy_evaluations ? 'grpc' THEN 'grpc' ELSE 'http' END`

// refusalWhere selects requests the gateway refused by policy: not proxied,
// and not failures of the upstream itself. MCP refusals are answered with
// HTTP 200 and a JSON-RPC error, so they are found by their trail.
const refusalWhere = `ts >= $1 AND ($2::text[] IS NULL OR tenant_id::text = ANY($2))
	AND decision_reason <> '' AND decision_reason NOT IN ('proxied_successfully', 'processing', 'client_closed_request')
	AND decision_reason NOT LIKE 'upstream%' AND decision_reason NOT LIKE 'grpc\_%'
	AND (status >= 400 OR policy_evaluations->'mcp' ? 'refused')`

// RefusalSummary reports what the gateway refused and why since a time,
// optionally for one protocol.
func (s *Store) RefusalSummary(ctx context.Context, since time.Time, tenantIDs []string, protocol string) ([]RefusalCount, []Refusal, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+refusalProtocol+` AS protocol, decision_reason, api_name,
			SUM(GREATEST(sample_weight, 1)), bool_or(policy_evaluations ? 'wasm'), MAX(ts)
		FROM request_logs WHERE `+refusalWhere+`
		GROUP BY 1, 2, 3 HAVING ($3 = '' OR `+refusalProtocol+` = $3)
		ORDER BY 4 DESC LIMIT 200`, since, tenantIDs, protocol)
	if err != nil {
		return nil, nil, err
	}
	counts := []RefusalCount{}
	for rows.Next() {
		var c RefusalCount
		if err := rows.Scan(&c.Protocol, &c.Reason, &c.APIName, &c.Count, &c.Plugin, &c.LastSeen); err != nil {
			rows.Close()
			return nil, nil, err
		}
		counts = append(counts, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	rows, err = s.Pool.Query(ctx, `SELECT ts, `+refusalProtocol+`, api_name, consumer_name, method, path, status,
			decision_reason, matched_route, error, policy_evaluations, request_id
		FROM request_logs WHERE `+refusalWhere+` AND ($3 = '' OR `+refusalProtocol+` = $3)
		ORDER BY ts DESC LIMIT 50`, since, tenantIDs, protocol)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	recent := []Refusal{}
	for rows.Next() {
		var r Refusal
		var evals []byte
		if err := rows.Scan(&r.TS, &r.Protocol, &r.APIName, &r.ConsumerName, &r.Method, &r.Path, &r.Status,
			&r.Reason, &r.Route, &r.Error, &evals, &r.RequestID); err != nil {
			return nil, nil, err
		}
		_ = json.Unmarshal(evals, &r.Evaluations)
		recent = append(recent, r)
	}
	return counts, recent, rows.Err()
}
