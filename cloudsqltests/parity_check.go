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

	fmt.Println("=== Parity Check: Sidecar vs Extension rc3 ===\n")

	// Total findings
	var totalFindings int
	conn.QueryRow(ctx, "SELECT count(*) FROM sage.findings WHERE status = 'open'").Scan(&totalFindings)
	fmt.Printf("Total open findings:  %d (extension: 32)\n", totalFindings)

	// By category
	fmt.Println("\nFindings by category:")
	rows, err := conn.Query(ctx, `
SELECT category, count(*) FROM sage.findings WHERE status = 'open'
GROUP BY category ORDER BY count(*) DESC`)
	if err != nil {
		log.Fatal(err)
	}
	for rows.Next() {
		var cat string
		var cnt int
		rows.Scan(&cat, &cnt)
		fmt.Printf("  %-25s %d\n", cat, cnt)
	}
	rows.Close()

	// Snapshot categories
	var catCount int
	conn.QueryRow(ctx, "SELECT count(DISTINCT category) FROM sage.snapshots").Scan(&catCount)
	fmt.Printf("\nSnapshot categories:  %d (extension: 6)\n", catCount)

	// Action count
	var actionCount int
	conn.QueryRow(ctx, "SELECT count(*) FROM sage.action_log").Scan(&actionCount)
	fmt.Printf("Tier 3 actions:       %d\n", actionCount)

	// CONCURRENTLY in DDL
	var concCount int
	conn.QueryRow(ctx, "SELECT count(*) FROM sage.action_log WHERE sql_executed LIKE '%CONCURRENTLY%'").Scan(&concCount)
	fmt.Printf("CONCURRENTLY in DDL:  %d/%d\n", concCount, actionCount)

	// LLM tokens
	var tokenCount int
	conn.QueryRow(ctx, "SELECT COALESCE(sum((detail->>'tokens_used')::int), 0) FROM sage.findings WHERE category = 'index_optimization'").Scan(&tokenCount)
	fmt.Printf("\nLLM metrics:\n")

	// Check findings detail
	fmt.Println("\nAll open findings:")
	rows, err = conn.Query(ctx, `
SELECT category, severity, object_identifier
FROM sage.findings WHERE status = 'open'
ORDER BY category, severity DESC`)
	if err != nil {
		log.Fatal(err)
	}
	for rows.Next() {
		var cat, sev, obj string
		rows.Scan(&cat, &sev, &obj)
		fmt.Printf("  [%s] %-25s %s\n", sev, cat, obj)
	}
	rows.Close()

	// Resolved findings (from Tier 3 actions)
	var resolvedCount int
	conn.QueryRow(ctx, "SELECT count(*) FROM sage.findings WHERE status = 'resolved'").Scan(&resolvedCount)
	fmt.Printf("\nResolved findings: %d\n", resolvedCount)
}
