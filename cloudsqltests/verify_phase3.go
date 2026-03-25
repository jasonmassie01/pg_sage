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

	// Phase 3: Snapshot categories
	fmt.Println("=== Phase 3: Snapshot Categories ===")
	rows, err := conn.Query(ctx, `
SELECT category, count(*), max(collected_at)
FROM sage.snapshots GROUP BY category ORDER BY category`)
	if err != nil {
		log.Fatalf("snapshots: %v", err)
	}
	for rows.Next() {
		var cat string
		var cnt int
		var maxTime interface{}
		rows.Scan(&cat, &cnt, &maxTime)
		fmt.Printf("  %-20s count=%d last=%v\n", cat, cnt, maxTime)
	}
	rows.Close()

	// Check no sage schema objects in snapshots
	var sageCount int
	conn.QueryRow(ctx, `
SELECT count(*) FROM sage.snapshots
WHERE data::text LIKE '%"sage.%' AND category NOT IN ('system')`).Scan(&sageCount)
	fmt.Printf("\n  sage schema objects in snapshots: %d (must be 0)\n", sageCount)

	// Phase 4: Findings
	fmt.Println("\n=== Phase 4: Findings ===")
	rows, err = conn.Query(ctx, `
SELECT category, severity, object_identifier, left(detail::text, 100) AS detail
FROM sage.findings WHERE status = 'open'
ORDER BY severity DESC, category`)
	if err != nil {
		log.Fatalf("findings: %v", err)
	}
	count := 0
	for rows.Next() {
		var cat, sev, obj, detail string
		rows.Scan(&cat, &sev, &obj, &detail)
		count++
		fmt.Printf("  [%s] %-25s %s\n    %s\n", sev, cat, obj, detail)
	}
	rows.Close()
	fmt.Printf("\n  Total open findings: %d\n", count)

	// Check no PK/unique in unused_index
	var pkCount int
	conn.QueryRow(ctx, `
SELECT count(*) FROM sage.findings
WHERE category = 'unused_index' AND status = 'open'
AND (object_identifier LIKE '%_pkey%' OR object_identifier LIKE '%_key%' OR object_identifier LIKE '%_unique%')`).Scan(&pkCount)
	fmt.Printf("  PK/unique in unused_index: %d (must be 0)\n", pkCount)

	// Check no sage.* in findings
	var sageFindingsCount int
	conn.QueryRow(ctx, `
SELECT count(*) FROM sage.findings
WHERE status = 'open' AND object_identifier LIKE 'sage.%'`).Scan(&sageFindingsCount)
	fmt.Printf("  sage.* in findings: %d (must be 0)\n", sageFindingsCount)

	// Phase 5: LLM metrics
	fmt.Println("\n=== Phase 5: LLM ===")
	var llmCount int
	conn.QueryRow(ctx, `
SELECT count(*) FROM sage.findings
WHERE category = 'index_optimization'`).Scan(&llmCount)
	fmt.Printf("  index_optimization findings: %d\n", llmCount)

	// Check action_log
	fmt.Println("\n=== Phase 6: Action Log ===")
	rows, err = conn.Query(ctx, `
SELECT id, left(recommended_sql, 80), outcome, error_message
FROM sage.action_log ORDER BY executed_at DESC LIMIT 10`)
	if err != nil {
		fmt.Printf("  action_log: %v\n", err)
	} else {
		for rows.Next() {
			var id int
			var sql, outcome string
			var errMsg *string
			rows.Scan(&id, &sql, &outcome, &errMsg)
			e := ""
			if errMsg != nil {
				e = *errMsg
			}
			fmt.Printf("  [%d] %s outcome=%s err=%s\n", id, sql, outcome, e)
		}
		rows.Close()
	}
}
