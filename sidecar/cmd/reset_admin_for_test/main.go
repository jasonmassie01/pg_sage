// Test helper: resets an admin password for browser test authentication.
// NOT for production use.
// The operator chose the password, so it clears must_change_password
// (apiauth-03): a generated bootstrap password would otherwise still hold
// the session to the change form.
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
	"github.com/pg-sage/sidecar/internal/sagepool"
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
	os.Exit(run(os.Args[1:], os.Stderr, productionResetDeps()))
}

// run is main without the process exit: 2 on a usage error, 1 on a failure
// (apiauth-14).
func run(args []string, stderr io.Writer, deps resetDeps) int {
	cfg, err := loadResetConfig(args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	rows, err := resetAdmin(context.Background(), cfg, deps)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "reset password for %s (rows=%d)\n", cfg.Email, rows)
	return 0
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
		open: openResetPool,
		close: func(pool *pgxpool.Pool) {
			pool.Close()
		},
		hash: auth.HashPassword,
		update: func(
			ctx context.Context, pool *pgxpool.Pool, hash, email string,
		) (int64, error) {
			tag, err := pool.Exec(ctx,
				"UPDATE sage.users SET password=$1, must_change_password=false "+
					"WHERE email=$2", hash, email)
			return tag.RowsAffected(), err
		},
	}
}

// openResetPool connects and pings: a pool connects lazily, so an
// unreachable server would otherwise surface at the first query.
func openResetPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := sagepool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}
