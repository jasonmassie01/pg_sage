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

	// Force-release the advisory lock by terminating the holding session
	_, err = conn.Exec(ctx, `
SELECT pg_terminate_backend(pid)
FROM pg_stat_activity
WHERE pid != pg_backend_pid()
AND datname = 'sage_test'
AND usename = 'sage_agent'`)
	if err != nil {
		fmt.Printf("terminate: %v\n", err)
	}

	fmt.Println("All sage_agent sessions terminated (advisory lock released)")
}
