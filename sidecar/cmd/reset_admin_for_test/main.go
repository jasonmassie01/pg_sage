// Test helper: resets an admin password for browser test authentication.
// NOT for production use.
//
//	go run ./cmd/reset_admin_for_test -dsn "$DSN" -email admin@pg-sage.local -password "$PW"
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

type resetConfig struct {
	DSN      string
	Email    string
	Password string
}

type resetDeps struct {
	open   func(context.Context, string) (*pgxpool.Pool, error)
	close  func(*pgxpool.Pool)
	hash   func(string) (string, error)
	update func(context.Context, *pgxpool.Pool, string, string) (int64, error)
}

func main() {
	cfg, err := loadResetConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	rows, err := resetAdmin(context.Background(), cfg, productionResetDeps())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "reset password for %s (rows=%d)\n", cfg.Email, rows)
}

func loadResetConfig(args []string) (resetConfig, error) {
	fs := flag.NewFlagSet("reset_admin_for_test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dsn := fs.String("dsn", "", "postgres DSN")
	email := fs.String("email", "admin@pg-sage.local", "admin email")
	password := fs.String("password", "", "new password (min 12 chars)")
	if err := fs.Parse(args); err != nil {
		return resetConfig{}, err
	}
	if *dsn == "" || *password == "" {
		return resetConfig{}, fmt.Errorf("dsn and password are required")
	}
	if len(*password) < 12 {
		return resetConfig{}, fmt.Errorf("password must be at least 12 characters")
	}
	return resetConfig{DSN: *dsn, Email: *email, Password: *password}, nil
}

func resetAdmin(
	ctx context.Context, cfg resetConfig, deps resetDeps,
) (int64, error) {
	pool, err := deps.open(ctx, cfg.DSN)
	if err != nil {
		return 0, fmt.Errorf("connect to admin database: %w", err)
	}
	defer deps.close(pool)
	hash, err := deps.hash(cfg.Password)
	if err != nil {
		return 0, fmt.Errorf("hash admin password: %w", err)
	}
	rows, err := deps.update(ctx, pool, hash, cfg.Email)
	if err != nil {
		return 0, fmt.Errorf("update admin password: %w", err)
	}
	if rows == 0 {
		return 0, fmt.Errorf("no user with email %q", cfg.Email)
	}
	return rows, nil
}

func productionResetDeps() resetDeps {
	return resetDeps{
		open: pgxpool.New,
		close: func(pool *pgxpool.Pool) {
			pool.Close()
		},
		hash: auth.HashPassword,
		update: func(
			ctx context.Context, pool *pgxpool.Pool, hash, email string,
		) (int64, error) {
			tag, err := pool.Exec(ctx,
				"UPDATE sage.users SET password=$1 WHERE email=$2", hash, email)
			return tag.RowsAffected(), err
		},
	}
}
