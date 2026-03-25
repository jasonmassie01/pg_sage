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
	url := os.Getenv("SAGE_DATABASE_URL")
	if url == "" {
		log.Fatal("SAGE_DATABASE_URL not set")
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close(ctx)

	var count int
	conn.QueryRow(ctx, "SELECT count(*) FROM sage.snapshots").Scan(&count)
	fmt.Printf("snapshots: %d\n", count)

	conn.QueryRow(ctx, "SELECT count(*) FROM sage.findings").Scan(&count)
	fmt.Printf("findings: %d\n", count)

	conn.QueryRow(ctx, "SELECT count(*) FROM sage.action_log").Scan(&count)
	fmt.Printf("actions: %d\n", count)

	rows, _ := conn.Query(ctx,
		"SELECT category, count(*) FROM sage.snapshots GROUP BY category ORDER BY category")
	defer rows.Close()
	fmt.Println("\nsnapshot categories:")
	for rows.Next() {
		var cat string
		var n int
		rows.Scan(&cat, &n)
		fmt.Printf("  %s: %d\n", cat, n)
	}

	rows2, _ := conn.Query(ctx,
		"SELECT category, severity, count(*) FROM sage.findings WHERE status='open' GROUP BY category, severity ORDER BY count(*) DESC LIMIT 20")
	defer rows2.Close()
	fmt.Println("\nopen findings:")
	for rows2.Next() {
		var cat, sev string
		var n int
		rows2.Scan(&cat, &sev, &n)
		fmt.Printf("  %s [%s]: %d\n", cat, sev, n)
	}

	rows3, _ := conn.Query(ctx,
		"SELECT action_type, outcome, count(*) FROM sage.action_log GROUP BY action_type, outcome ORDER BY count(*) DESC")
	defer rows3.Close()
	fmt.Println("\naction log:")
	for rows3.Next() {
		var at, out string
		var n int
		rows3.Scan(&at, &out, &n)
		fmt.Printf("  %s [%s]: %d\n", at, out, n)
	}
}
