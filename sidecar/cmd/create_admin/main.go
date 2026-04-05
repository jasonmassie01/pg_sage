package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

func main() {
	dsn := os.Getenv("PG_SAGE_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "PG_SAGE_DSN environment variable must be set")
		os.Exit(1)
	}

	password := os.Getenv("PG_SAGE_ADMIN_PASSWORD")
	if password == "" {
		fmt.Fprintln(os.Stderr, "PG_SAGE_ADMIN_PASSWORD environment variable must be set")
		os.Exit(1)
	}
	if len(password) < 12 {
		fmt.Fprintln(os.Stderr, "PG_SAGE_ADMIN_PASSWORD must be at least 12 characters")
		os.Exit(1)
	}

	email := os.Getenv("PG_SAGE_ADMIN_EMAIL")
	if email == "" {
		email = "admin@pg-sage.local"
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pool.Close()

	id, err := auth.CreateUser(ctx, pool, email, password, "admin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr,
		"Created admin user id=%d\n  email: %s\n  password: [set via PG_SAGE_ADMIN_PASSWORD]\n",
		id, email,
	)
}
