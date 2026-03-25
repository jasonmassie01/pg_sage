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

	// Check bloat/vacuum findings
	fmt.Println("=== Vacuum/Bloat Findings ===")
	rows, err := conn.Query(ctx, `
SELECT id, category, object_identifier, severity, status, recommended_sql, severity
FROM sage.findings
WHERE category IN ('table_bloat', 'vacuum', 'dead_tuples')
   OR recommended_sql LIKE '%VACUUM%'
ORDER BY id`)
	if err != nil {
		log.Fatal(err)
	}
	for rows.Next() {
		var id int
		var cat, obj, sev, status string
		var recSql, risk *string
		rows.Scan(&id, &cat, &obj, &sev, &status, &recSql, &risk)
		s := ""
		if recSql != nil {
			s = *recSql
		}
		r := ""
		if risk != nil {
			r = *risk
		}
		fmt.Printf("  [%d] %s %s sev=%s status=%s risk=%s sql=%s\n", id, cat, obj, sev, status, r, s)
	}
	rows.Close()

	// Check all findings with recommended_sql
	fmt.Println("\n=== All Actionable Findings ===")
	rows, err = conn.Query(ctx, `
SELECT id, category, object_identifier, severity, status, left(recommended_sql, 100)
FROM sage.findings
WHERE recommended_sql IS NOT NULL AND recommended_sql != ''
ORDER BY id`)
	if err != nil {
		log.Fatal(err)
	}
	for rows.Next() {
		var id int
		var cat, obj, status string
		var risk, sql *string
		rows.Scan(&id, &cat, &obj, &risk, &status, &sql)
		r := ""
		if risk != nil {
			r = *risk
		}
		s := ""
		if sql != nil {
			s = *sql
		}
		fmt.Printf("  [%d] %s %s risk=%s status=%s sql=%s\n", id, cat, obj, r, status, s)
	}
	rows.Close()
}
