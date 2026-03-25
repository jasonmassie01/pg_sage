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

	// Backdate trust ramp 32 days
	_, err = conn.Exec(ctx, `
INSERT INTO sage.config (key, value)
VALUES ('trust_ramp_start', (now() - interval '32 days')::text)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`)
	if err != nil {
		log.Fatalf("backdate: %v", err)
	}
	fmt.Println("trust_ramp_start backdated 32 days")

	// Verify advisory lock
	var lockObjID int64
	var granted bool
	err = conn.QueryRow(ctx, `
SELECT objid, granted FROM pg_locks
WHERE locktype = 'advisory' LIMIT 1`).Scan(&lockObjID, &granted)
	if err != nil {
		fmt.Printf("advisory lock check: %v (may be no advisory locks)\n", err)
	} else {
		fmt.Printf("advisory lock: objid=%d granted=%v\n", lockObjID, granted)
	}

	// Check sage schema tables
	rows, err := conn.Query(ctx, `
SELECT table_name FROM information_schema.tables
WHERE table_schema = 'sage' ORDER BY table_name`)
	if err != nil {
		log.Fatalf("check tables: %v", err)
	}
	defer rows.Close()

	fmt.Println("\nsage schema tables:")
	for rows.Next() {
		var name string
		rows.Scan(&name)
		fmt.Printf("  %s\n", name)
	}

	// Check trust ramp value
	var val string
	conn.QueryRow(ctx, "SELECT value FROM sage.config WHERE key = 'trust_ramp_start'").Scan(&val)
	fmt.Printf("\ntrust_ramp_start: %s\n", val)
}
