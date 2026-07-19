package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLoadResetConfigValidatesFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing", args: nil, want: "dsn and password"},
		{name: "short", args: []string{
			"-dsn", "postgres://test", "-password", "short",
		}, want: "at least 12"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadResetConfig(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("loadResetConfig error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestResetAdminRunsHashUpdateAndClosesPool(t *testing.T) {
	cfg := resetConfig{
		DSN: "postgres://test", Email: "admin@example.com", Password: "secret",
	}
	var events []string
	deps := resetDeps{
		open: func(_ context.Context, dsn string) (*pgxpool.Pool, error) {
			events = append(events, "open:"+dsn)
			return nil, nil
		},
		close: func(_ *pgxpool.Pool) { events = append(events, "close") },
		hash: func(password string) (string, error) {
			events = append(events, "hash:"+password)
			return "hash", nil
		},
		update: func(_ context.Context, _ *pgxpool.Pool, hash, email string) (int64, error) {
			events = append(events, "update:"+hash+":"+email)
			return 1, nil
		},
	}
	rows, err := resetAdmin(context.Background(), cfg, deps)
	if err != nil || rows != 1 {
		t.Fatalf("resetAdmin = (%d, %v), want (1, nil)", rows, err)
	}
	want := []string{
		"open:postgres://test", "hash:secret", "update:hash:admin@example.com", "close",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}
