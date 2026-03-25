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

	action := os.Args[1]

	switch action {
	case "setup":
		fmt.Print("Creating shutdown_test table (5M rows)... ")
		_, err = conn.Exec(ctx, `
CREATE TABLE IF NOT EXISTS shutdown_test AS
SELECT generate_series(1,5000000) AS id, repeat('x',100) AS data`)
		if err != nil {
			fmt.Printf("ERROR: %v\n", err)
			return
		}
		fmt.Println("OK")

		fmt.Print("Injecting slow DDL finding... ")
		_, err = conn.Exec(ctx, `
INSERT INTO sage.findings (category, object_identifier, severity, status, recommended_sql,
    title, detail, created_at, last_seen, occurrence_count)
VALUES ('missing_index', 'shutdown_test.id', 'warning', 'open',
        'CREATE INDEX CONCURRENTLY idx_shutdown_test ON shutdown_test (data)',
        'Missing index on shutdown_test.data',
        '{"table": "shutdown_test"}'::jsonb,
        now() - interval '60 days', now(), 10)`)
		if err != nil {
			fmt.Printf("ERROR: %v\n", err)
			return
		}
		fmt.Println("OK")

	case "verify":
		// Check if the index exists and is valid
		var exists bool
		err = conn.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE indexname = 'idx_shutdown_test')`).Scan(&exists)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("idx_shutdown_test exists: %v\n", exists)

		if exists {
			var valid bool
			conn.QueryRow(ctx, `
SELECT indisvalid FROM pg_index WHERE indexrelid = 'idx_shutdown_test'::regclass`).Scan(&valid)
			fmt.Printf("idx_shutdown_test valid: %v\n", valid)
		}

		// Check advisory lock
		var lockCount int
		conn.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'").Scan(&lockCount)
		fmt.Printf("Advisory locks: %d\n", lockCount)

	case "cleanup":
		conn.Exec(ctx, "DROP TABLE IF EXISTS shutdown_test CASCADE")
		fmt.Println("Cleanup done")
	}
}
