package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLoadAdminConfigValidatesRequiredEnvironment(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "missing dsn", env: map[string]string{}, want: "PG_SAGE_DSN"},
		{name: "missing password", env: map[string]string{
			"PG_SAGE_DSN": "postgres://test",
		}, want: "PG_SAGE_ADMIN_PASSWORD"},
		{name: "short password", env: map[string]string{
			"PG_SAGE_DSN": "postgres://test", "PG_SAGE_ADMIN_PASSWORD": "short",
		}, want: "at least 12"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadAdminConfig(mapEnv(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("loadAdminConfig error = %v, want %q", err, tc.want)
			}
		})
	}

	cfg, err := loadAdminConfig(mapEnv(map[string]string{
		"PG_SAGE_DSN":            "postgres://test",
		"PG_SAGE_ADMIN_PASSWORD": "long-enough-password",
	}))
	if err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if cfg.Email != "admin@pg-sage.local" {
		t.Fatalf("default email = %q", cfg.Email)
	}
}

func TestCreateAdminRunsSequenceAndClosesPool(t *testing.T) {
	cfg := adminConfig{DSN: "postgres://test", Email: "admin@example.com", Password: "secret"}
	var events []string
	deps := adminDeps{
		open: func(_ context.Context, dsn string) (*pgxpool.Pool, error) {
			events = append(events, "open:"+dsn)
			return nil, nil
		},
		close: func(_ *pgxpool.Pool) { events = append(events, "close") },
		resetSequence: func(_ context.Context, _ *pgxpool.Pool) error {
			events = append(events, "reset")
			return nil
		},
		createUser: func(
			_ context.Context, _ *pgxpool.Pool, email, password, role string,
		) (int, error) {
			events = append(events, email+":"+password+":"+role)
			return 42, nil
		},
	}
	id, err := createAdmin(context.Background(), cfg, deps)
	if err != nil || id != 42 {
		t.Fatalf("createAdmin = (%d, %v), want (42, nil)", id, err)
	}
	want := []string{
		"open:postgres://test", "reset", "admin@example.com:secret:admin", "close",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func mapEnv(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}
