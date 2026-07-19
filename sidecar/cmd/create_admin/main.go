package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

type adminConfig struct {
	DSN      string
	Email    string
	Password string
}

type adminDeps struct {
	open          func(context.Context, string) (*pgxpool.Pool, error)
	close         func(*pgxpool.Pool)
	resetSequence func(context.Context, *pgxpool.Pool) error
	createUser    func(context.Context, *pgxpool.Pool, string, string, string) (int, error)
}

func main() {
	cfg, err := loadAdminConfig(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	id, err := createAdmin(context.Background(), cfg, productionAdminDeps())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr,
		"Created admin user id=%d\n  email: %s\n"+
			"  password: [set via PG_SAGE_ADMIN_PASSWORD]\n",
		id, cfg.Email,
	)
}

func loadAdminConfig(getenv func(string) string) (adminConfig, error) {
	cfg := adminConfig{
		DSN: getenv("PG_SAGE_DSN"), Email: getenv("PG_SAGE_ADMIN_EMAIL"),
		Password: getenv("PG_SAGE_ADMIN_PASSWORD"),
	}
	if cfg.DSN == "" {
		return adminConfig{}, fmt.Errorf("PG_SAGE_DSN environment variable must be set")
	}
	if cfg.Password == "" {
		return adminConfig{}, fmt.Errorf(
			"PG_SAGE_ADMIN_PASSWORD environment variable must be set")
	}
	if len(cfg.Password) < 12 {
		return adminConfig{}, fmt.Errorf(
			"PG_SAGE_ADMIN_PASSWORD must be at least 12 characters")
	}
	if cfg.Email == "" {
		cfg.Email = "admin@pg-sage.local"
	}
	return cfg, nil
}

func createAdmin(
	ctx context.Context, cfg adminConfig, deps adminDeps,
) (int, error) {
	pool, err := deps.open(ctx, cfg.DSN)
	if err != nil {
		return 0, fmt.Errorf("connect to admin database: %w", err)
	}
	defer deps.close(pool)
	if err := deps.resetSequence(ctx, pool); err != nil {
		return 0, fmt.Errorf("reset sage.users sequence: %w", err)
	}
	id, err := deps.createUser(ctx, pool, cfg.Email, cfg.Password, "admin")
	if err != nil {
		return 0, fmt.Errorf("create admin user: %w", err)
	}
	return id, nil
}

func productionAdminDeps() adminDeps {
	return adminDeps{
		open: pgxpool.New,
		close: func(pool *pgxpool.Pool) {
			pool.Close()
		},
		resetSequence: func(ctx context.Context, pool *pgxpool.Pool) error {
			_, err := pool.Exec(ctx, `SELECT setval(
				pg_get_serial_sequence('sage.users','id'),
				COALESCE((SELECT max(id) FROM sage.users), 0)+1,
				false)`)
			return err
		},
		createUser: auth.CreateUser,
	}
}
