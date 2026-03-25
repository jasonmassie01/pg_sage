// +build ignore

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()
	conn, _ := pgx.Connect(ctx, os.Getenv("SAGE_DATABASE_URL"))
	defer conn.Close(ctx)
	var count int
	conn.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'").Scan(&count)
	fmt.Printf("Advisory locks: %d (must be 0)\n", count)
}
