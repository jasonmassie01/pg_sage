// Test helper: resets the admin@pg-sage.local password to a known value
// so Playwright specs can authenticate. NOT for production use.
//
//	go run ./cmd/reset_admin_for_test -dsn "$DSN" -email admin@pg-sage.local -password "$PW"
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

func main() {
	dsn := flag.String("dsn", "", "postgres DSN")
	email := flag.String("email", "admin@pg-sage.local", "admin email")
	password := flag.String("password", "", "new password (min 12 chars)")
	flag.Parse()

	if *dsn == "" || *password == "" {
		fmt.Fprintln(os.Stderr, "-dsn and -password are required")
		os.Exit(2)
	}
	if len(*password) < 12 {
		fmt.Fprintln(os.Stderr, "-password must be at least 12 chars")
		os.Exit(2)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pool.Close()

	hash, err := auth.HashPassword(*password)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	tag, err := pool.Exec(ctx,
		"UPDATE sage.users SET password=$1 WHERE email=$2",
		hash, *email,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if tag.RowsAffected() == 0 {
		fmt.Fprintf(os.Stderr, "no user with email %q\n", *email)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "reset password for %s (rows=%d)\n", *email, tag.RowsAffected())
}
