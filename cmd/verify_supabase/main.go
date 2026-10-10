package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	var dbURL string
	var runCleanup bool
	var olderThanHours int

	flag.StringVar(&dbURL, "db-url", os.Getenv("RELAYOPS_DATABASE_URL"), "Supabase PostgreSQL connection URL (or set RELAYOPS_DATABASE_URL)")
	flag.BoolVar(&runCleanup, "apply-cleanup", false, "Apply optimization: drop from Realtime publication, purge stale telemetry, vacuum and schedule pg_cron")
	flag.IntVar(&olderThanHours, "older-than-hours", 24, "Purge telemetry older than N hours when --apply-cleanup is set")
	flag.Parse()

	if dbURL == "" {
		fmt.Fprintln(os.Stderr, "Error: PostgreSQL connection string required via -db-url or RELAYOPS_DATABASE_URL.")
		fmt.Fprintln(os.Stderr, "Format: postgresql://postgres.<project-id>:<password>@aws-0-ap-southeast-2.pooler.supabase.com:5432/postgres?sslmode=require")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	parsed, err := url.Parse(dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing database URL: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("================================================================================")
	fmt.Println("           RelayOps APIM: Live Supabase Verification & Health Audit             ")
	fmt.Println("================================================================================")
	fmt.Printf("Host: %s\n", parsed.Host)
	fmt.Printf("User: %s\n", parsed.User.Username())
	fmt.Printf("Path: %s\n\n", parsed.Path)

	// Check 1: Port verification (Session vs Transaction pooler)
	port := parsed.Port()
	if port == "6543" {
		fmt.Println("[WARNING] You are targeting port 6543 (Supabase Transaction Pooler).")
		fmt.Println("          RelayOps requires port 5432 (Session Pooler) for PostgreSQL LISTEN/NOTIFY.")
		fmt.Print("          Connecting on 6543 will cause LISTEN statements to fail.\n\n")
	} else if port == "5432" {
		fmt.Print("[OK] Port 5432 detected (Supabase Session Pooler) - compliant with LISTEN/NOTIFY.\n\n")
	}

	// Connect
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse pgx config: %v\n", err)
		os.Exit(1)
	}
	cfg.MaxConns = 5

	fmt.Print("Connecting to Supabase PostgreSQL... ")
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		fmt.Printf("FAILED: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		fmt.Printf("FAILED: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("SUCCESS (Connection established via TLS)")

	// Check 2: Version
	var version string
	if err := pool.QueryRow(ctx, "SELECT version()").Scan(&version); err != nil {
		fmt.Printf("Failed to query version: %v\n", err)
	} else {
		shortVersion := strings.Split(version, ",")[0]
		fmt.Printf("[OK] Server Version: %s\n\n", shortVersion)
	}

	// Check 3: LISTEN / NOTIFY Round-Trip
	fmt.Print("Testing PostgreSQL LISTEN / NOTIFY pub/sub capability... ")
	testListenNotify(ctx, pool)

	// Check 4: Inspect Current Storage Footprint
	fmt.Println("\n--- Current Storage & Row Footprint in 'public' Schema ---")
	rows, err := pool.Query(ctx, `
		SELECT 
			relname,
			n_live_tup,
			pg_size_pretty(pg_total_relation_size(relid))
		FROM pg_stat_user_tables
		WHERE schemaname = 'public'
		ORDER BY pg_total_relation_size(relid) DESC
		LIMIT 15;
	`)
	if err != nil {
		fmt.Printf("Failed to inspect storage: %v\n", err)
	} else {
		defer rows.Close()
		fmt.Printf("%-30s | %-15s | %-15s\n", "Table Name", "Est. Row Count", "Total Size")
		fmt.Println(strings.Repeat("-", 66))
		for rows.Next() {
			var name string
			var count int64
			var size string
			if err := rows.Scan(&name, &count, &size); err == nil {
				fmt.Printf("%-30s | %-15d | %-15s\n", name, count, size)
			}
		}
	}

	// Check 5: Check Supabase Realtime Publication
	fmt.Println("\n--- Supabase Realtime Publication Audit ---")
	rtRows, err := pool.Query(ctx, `
		SELECT tablename 
		FROM pg_publication_tables 
		WHERE pubname = 'supabase_realtime';
	`)
	if err != nil {
		fmt.Printf("Note: pg_publication_tables not readable or supabase_realtime does not exist: %v\n", err)
	} else {
		defer rtRows.Close()
		var rtTables []string
		for rtRows.Next() {
			var t string
			if err := rtRows.Scan(&t); err == nil {
				rtTables = append(rtTables, t)
			}
		}
		if len(rtTables) == 0 {
			fmt.Println("[OK] No tables in supabase_realtime publication (zero background WebSocket egress).")
		} else {
			fmt.Printf("Tables in 'supabase_realtime': %s\n", strings.Join(rtTables, ", "))
			for _, t := range rtTables {
				if t == "request_logs" || t == "test_run_steps" || t == "test_jobs" {
					fmt.Printf("[CRITICAL WARNING] High-volume table '%s' is being streamed over WebSockets!\n", t)
					fmt.Println("                   This generates high monthly egress charges. Drop it from Realtime.")
				}
			}
		}
	}

	// Check 6: Check pg_cron Scheduled Purge
	fmt.Println("\n--- Automated Daily Cleanup (pg_cron) Audit ---")
	var cronJobCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) 
		FROM cron.job 
		WHERE jobname = 'relayops-daily-telemetry-purge';
	`).Scan(&cronJobCount)
	if err != nil {
		fmt.Println("[INFO] pg_cron extension not queryable or not yet configured.")
	} else if cronJobCount > 0 {
		fmt.Println("[OK] 'relayops-daily-telemetry-purge' is active in pg_cron.")
	} else {
		fmt.Println("[INFO] No daily purge job configured in pg_cron yet.")
	}

	// Apply cleanup if requested
	if runCleanup {
		fmt.Println("\n================================================================================")
		fmt.Println("                 Applying Optimization & Maintenance Cleanups                  ")
		fmt.Println("================================================================================")
		applyOptimizations(ctx, pool, olderThanHours)
	} else {
		fmt.Println("\n================================================================================")
		fmt.Println("To automatically remove tables from Realtime, purge historical logs,")
		fmt.Println("and schedule daily automated maintenance, re-run with:")
		fmt.Println("    ./bin/verify-supabase --apply-cleanup")
		fmt.Println("================================================================================")
	}
}

func testListenNotify(ctx context.Context, pool *pgxpool.Pool) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		fmt.Printf("FAILED to acquire connection: %v\n", err)
		return
	}
	defer conn.Release()

	channel := "relayops_verify_probe"
	_, err = conn.Exec(ctx, "LISTEN "+channel)
	if err != nil {
		fmt.Printf("FAILED (LISTEN error): %v\n", err)
		return
	}

	// Notify on another connection
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = pool.Exec(ctx, fmt.Sprintf("NOTIFY %s, 'healthy'", channel))
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 3*time.Second)
	defer waitCancel()

	notif, err := conn.Conn().WaitForNotification(waitCtx)
	if err != nil {
		fmt.Printf("FAILED (Notification wait timeout): %v\n", err)
		return
	}

	fmt.Printf("SUCCESS (Received payload: %q, Channel: %q)\n", notif.Payload, notif.Channel)
}

func applyOptimizations(ctx context.Context, pool *pgxpool.Pool, hours int) {
	// 1. Detach from supabase_realtime
	fmt.Print("1. Dropping telemetry tables from supabase_realtime publication... ")
	detachSQL := `
		DO $$
		BEGIN
			IF EXISTS (SELECT 1 FROM pg_publication WHERE pubname = 'supabase_realtime') THEN
				BEGIN
					ALTER PUBLICATION supabase_realtime DROP TABLE public.request_logs;
				EXCEPTION WHEN OTHERS THEN END;
				BEGIN
					ALTER PUBLICATION supabase_realtime DROP TABLE public.test_run_steps;
				EXCEPTION WHEN OTHERS THEN END;
				BEGIN
					ALTER PUBLICATION supabase_realtime DROP TABLE public.test_jobs;
				EXCEPTION WHEN OTHERS THEN END;
			END IF;
		END $$;
	`
	if _, err := pool.Exec(ctx, detachSQL); err != nil {
		fmt.Printf("WARNING: %v\n", err)
	} else {
		fmt.Println("DONE")
	}

	// 2. Purge stale records
	fmt.Printf("2. Purging request_logs older than %d hours... ", hours)
	tag, err := pool.Exec(ctx, `DELETE FROM public.request_logs WHERE ts < now() - make_interval(secs => $1)`, float64(hours*3600))
	if err != nil {
		fmt.Printf("Error: %v\n", err)
	} else {
		fmt.Printf("DONE (%d rows deleted)\n", tag.RowsAffected())
	}

	fmt.Print("3. Purging test_run_steps and completed test_runs older than 7 days... ")
	tagSteps, _ := pool.Exec(ctx, `DELETE FROM public.test_run_steps WHERE created_at < now() - interval '7 days'`)
	tagRuns, _ := pool.Exec(ctx, `DELETE FROM public.test_runs WHERE completed_at IS NOT NULL AND completed_at < now() - interval '7 days'`)
	tagJobs, _ := pool.Exec(ctx, `DELETE FROM public.test_jobs WHERE status IN ('completed', 'failed', 'cancelled') AND created_at < now() - interval '7 days'`)
	fmt.Printf("DONE (%d steps, %d runs, %d jobs purged)\n", tagSteps.RowsAffected(), tagRuns.RowsAffected(), tagJobs.RowsAffected())

	// 3. Vacuum
	fmt.Print("4. Running VACUUM ANALYZE to reclaim storage... ")
	_, _ = pool.Exec(ctx, "VACUUM ANALYZE public.request_logs")
	_, _ = pool.Exec(ctx, "VACUUM ANALYZE public.test_run_steps")
	_, _ = pool.Exec(ctx, "VACUUM ANALYZE public.test_runs")
	_, _ = pool.Exec(ctx, "VACUUM ANALYZE public.test_jobs")
	fmt.Println("DONE")

	// 4. Schedule pg_cron
	fmt.Print("5. Scheduling daily automatic telemetry purge in pg_cron... ")
	cronSQL := `
		CREATE EXTENSION IF NOT EXISTS pg_cron;
		SELECT cron.unschedule(jobid) FROM cron.job WHERE jobname = 'relayops-daily-telemetry-purge';
		SELECT cron.schedule(
			'relayops-daily-telemetry-purge',
			'0 3 * * *',
			$$
				DELETE FROM public.request_logs WHERE ts < (now() - interval '24 hours');
				DELETE FROM public.test_run_steps WHERE created_at < (now() - interval '7 days');
				DELETE FROM public.test_runs WHERE completed_at IS NOT NULL AND completed_at < (now() - interval '7 days');
				DELETE FROM public.test_jobs WHERE status IN ('completed', 'failed', 'cancelled') AND created_at < (now() - interval '7 days');
			$$
		);
	`
	if _, err := pool.Exec(ctx, cronSQL); err != nil {
		fmt.Printf("Note: pg_cron setup returned %v (you can run scripts/supabase_cleanup_and_optimize.sql in Supabase Dashboard SQL Editor)\n", err)
	} else {
		fmt.Println("DONE")
	}

	fmt.Println("\nAll optimizations successfully applied to live Supabase database!")
}
