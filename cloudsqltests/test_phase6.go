// +build ignore

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("SAGE_DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close(ctx)

	action := os.Args[1]

	switch action {
	case "emergency-stop":
		// Set emergency stop
		_, err = conn.Exec(ctx, `
INSERT INTO sage.config (key, value)
VALUES ('emergency_stop', 'true')
ON CONFLICT (key) DO UPDATE SET value = 'true'`)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("Emergency stop ENABLED")

	case "emergency-resume":
		_, err = conn.Exec(ctx, `
UPDATE sage.config SET value = 'false' WHERE key = 'emergency_stop'`)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("Emergency stop DISABLED")

	case "check-emergency":
		// Count actions in last 5 minutes
		var count int
		conn.QueryRow(ctx, `
SELECT count(*) FROM sage.action_log
WHERE executed_at > now() - interval '5 minutes'`).Scan(&count)
		fmt.Printf("Actions in last 5 min: %d (must be 0 during stop)\n", count)

	case "inject-fail":
		// Insert a finding that references a non-existent index
		_, err = conn.Exec(ctx, `
INSERT INTO sage.findings (category, object_identifier, severity, status, recommended_sql,
    title, detail, created_at, last_seen, occurrence_count)
VALUES ('unused_index', 'public.idx_this_does_not_exist', 'warning', 'open',
        'DROP INDEX CONCURRENTLY IF EXISTS public.idx_this_does_not_exist',
        'Unused index: idx_this_does_not_exist',
        '{"size": 0, "table": "test", "index_def": "CREATE INDEX idx_this_does_not_exist ON test (id)"}'::jsonb,
        now() - interval '60 days', now(), 5)`)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("Injected failing finding")

	case "check-fail-retry":
		// Count attempts for the failing finding
		var count int
		conn.QueryRow(ctx, `
SELECT count(*) FROM sage.action_log
WHERE finding_id = (SELECT id FROM sage.findings WHERE object_identifier = 'public.idx_this_does_not_exist' LIMIT 1)`).Scan(&count)
		fmt.Printf("Attempts for failing finding: %d (must be <= 1)\n", count)

	case "check-actions":
		fmt.Println("=== Action Log ===")
		rows, err := conn.Query(ctx, `
SELECT id, action_type, finding_id, left(sql_executed, 80), outcome, executed_at
FROM sage.action_log ORDER BY executed_at DESC LIMIT 20`)
		if err != nil {
			log.Fatal(err)
		}
		for rows.Next() {
			var id, fid int
			var atype, sql, outcome string
			var execAt time.Time
			rows.Scan(&id, &atype, &fid, &sql, &outcome, &execAt)
			fmt.Printf("  [%d] %s finding=%d outcome=%s at=%s\n    %s\n", id, atype, fid, outcome, execAt.Format("15:04:05"), sql)
		}
		rows.Close()

		// Check rollback monitoring
		var rollbackCount int
		conn.QueryRow(ctx, `SELECT count(*) FROM sage.action_log WHERE outcome = 'rolled_back'`).Scan(&rollbackCount)
		fmt.Printf("\n  Rolled-back actions: %d\n", rollbackCount)

		// Check index state
		fmt.Println("\n=== Index Verification ===")
		rows, err = conn.Query(ctx, `
SELECT indexname FROM pg_indexes WHERE tablename IN ('orders', 'line_items', 'order_events')
ORDER BY tablename, indexname`)
		if err != nil {
			log.Fatal(err)
		}
		for rows.Next() {
			var name string
			rows.Scan(&name)
			fmt.Printf("  %s\n", name)
		}
		rows.Close()

		// Check VACUUM status
		fmt.Println("\n=== Bloat Status ===")
		rows, err = conn.Query(ctx, `
SELECT relname, n_dead_tup, last_vacuum FROM pg_stat_user_tables
WHERE relname IN ('orders', 'order_events')`)
		if err != nil {
			log.Fatal(err)
		}
		for rows.Next() {
			var name string
			var dead int64
			var lastVac interface{}
			rows.Scan(&name, &dead, &lastVac)
			fmt.Printf("  %s: dead=%d last_vacuum=%v\n", name, dead, lastVac)
		}
		rows.Close()
	}
}
