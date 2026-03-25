// +build ignore

package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("SAGE_DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close(ctx)

	// Get action_log columns first
	rows, err := conn.Query(ctx, `
SELECT column_name FROM information_schema.columns
WHERE table_schema = 'sage' AND table_name = 'action_log'
ORDER BY ordinal_position`)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("=== action_log columns ===")
	for rows.Next() {
		var col string
		rows.Scan(&col)
		fmt.Printf("  %s\n", col)
	}
	rows.Close()

	// Query actions
	fmt.Println("\n=== Actions ===")
	rows, err = conn.Query(ctx, `
SELECT id, finding_id, sql_executed, outcome, rollback_reason, executed_at, action_type
FROM sage.action_log ORDER BY executed_at DESC LIMIT 20`)
	if err != nil {
		log.Fatalf("actions: %v", err)
	}
	for rows.Next() {
		var id, findingID int
		var sql, outcome string
		var reason *string
		var execAt interface{}
		var actionType string
		rows.Scan(&id, &findingID, &sql, &outcome, &reason, &execAt, &actionType)
		r := ""
		if reason != nil {
			r = *reason
		}
		fmt.Printf("  [%d] type=%s finding=%d outcome=%s at=%v\n    SQL: %s\n    reason: %s\n", id, actionType, findingID, outcome, execAt, sql, r)
	}
	rows.Close()

	// Check if duplicate indexes were actually dropped
	fmt.Println("\n=== Index Verification ===")
	var idxCount int
	conn.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_li_product_dup'`).Scan(&idxCount)
	fmt.Printf("  idx_li_product_dup exists: %v\n", idxCount > 0)

	conn.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_li_order'`).Scan(&idxCount)
	fmt.Printf("  idx_li_order exists: %v\n", idxCount > 0)

	conn.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_li_product'`).Scan(&idxCount)
	fmt.Printf("  idx_li_product exists: %v\n", idxCount > 0)

	// Check for VACUUM evidence
	fmt.Println("\n=== Bloat Status ===")
	rows, err = conn.Query(ctx, `
SELECT relname, n_dead_tup, last_vacuum, last_autovacuum
FROM pg_stat_user_tables
WHERE relname IN ('orders', 'order_events')`)
	if err != nil {
		log.Fatal(err)
	}
	for rows.Next() {
		var name string
		var dead int64
		var lastVac, lastAutoVac interface{}
		rows.Scan(&name, &dead, &lastVac, &lastAutoVac)
		fmt.Printf("  %s: dead=%d last_vacuum=%v last_autovacuum=%v\n", name, dead, lastVac, lastAutoVac)
	}
	rows.Close()

	// Check for new indexes created
	fmt.Println("\n=== New Indexes on orders ===")
	rows, err = conn.Query(ctx, `
SELECT indexname, indexdef FROM pg_indexes
WHERE tablename = 'orders' ORDER BY indexname`)
	if err != nil {
		log.Fatal(err)
	}
	for rows.Next() {
		var name, def string
		rows.Scan(&name, &def)
		fmt.Printf("  %s: %s\n", name, def)
	}
	rows.Close()
}
