package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// request_logs is partitioned by day: replays stay idempotent on (log_id,
// ts), and retention drops whole partitions.
func TestIntegrationRequestLogPartitioning(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	var partitioned bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_partitioned_table pt JOIN pg_class c ON c.oid = pt.partrelid WHERE c.relname = 'request_logs')`).Scan(&partitioned); err != nil || !partitioned {
		t.Fatalf("request_logs is not partitioned (%v)", err)
	}
	today := time.Now().UTC()
	var parts int
	_ = s.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_inherits i JOIN pg_class p ON p.oid = i.inhparent WHERE p.relname = 'request_logs'`).Scan(&parts)
	if parts < 8 { // default + today + a week ahead
		t.Fatalf("only %d partitions", parts)
	}

	old := today.AddDate(0, 0, -10)
	if _, err := s.Pool.Exec(ctx, `SELECT relayops_ensure_log_partitions($1::date, $1::date)`, old.Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	logs := []RequestLog{
		{TS: today, NodeID: "n1", LogID: "00000000-0000-4000-8000-0000000000a1", Method: "GET", Path: "/", Status: 200, ConfigRevision: 1},
		{TS: old, NodeID: "n1", LogID: "00000000-0000-4000-8000-0000000000a2", Method: "GET", Path: "/", Status: 200, ConfigRevision: 1},
		{TS: today.AddDate(1, 0, 0), NodeID: "n1", LogID: "00000000-0000-4000-8000-0000000000a3", Method: "GET", Path: "/", Status: 200, ConfigRevision: 1}, // beyond partitions: default
	}
	for i := 0; i < 2; i++ { // second insert is a replay
		if err := s.InsertLogs(ctx, logs); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	count := func(table string) int {
		var n int
		if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count("request_logs"); n != 3 {
		t.Fatalf("%d rows after a replay, want 3", n)
	}
	oldPart := fmt.Sprintf("request_logs_p%s", old.Format("20060102"))
	if count(oldPart) != 1 || count("request_logs_default") != 1 {
		t.Fatal("rows not routed to their day partition / the default partition")
	}

	removed, err := s.PurgeLogs(ctx, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("purged %d rows, want 1", removed)
	}
	var exists bool
	_ = s.Pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, oldPart).Scan(&exists)
	if exists {
		t.Fatalf("old partition %s was not dropped", oldPart)
	}
	if n := count("request_logs"); n != 2 {
		t.Fatalf("%d rows after purge, want 2", n)
	}
}
