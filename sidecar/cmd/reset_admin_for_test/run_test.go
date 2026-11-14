package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// apiauth-14: run is main's body; these tests pin its exit codes and messages.
// No concurrent access here: run keeps no shared state; main_db_test.go runs
// concurrent resets against a real database.

type fakeReset struct {
	openErr, hashErr, updateErr error
	rows                        int64
	opened, closed              int
	hashed, hash, email         string
}

func (f *fakeReset) deps() resetDeps {
	return resetDeps{
		open: func(context.Context, string) (*pgxpool.Pool, error) {
			f.opened++
			return nil, f.openErr
		},
		close: func(*pgxpool.Pool) { f.closed++ },
		hash: func(password string) (string, error) {
			f.hashed = password
			return "hashed:" + password, f.hashErr
		},
		update: func(
			_ context.Context, _ *pgxpool.Pool, hash, email string,
		) (int64, error) {
			f.hash, f.email = hash, email
			return f.rows, f.updateErr
		},
	}
}

const goodPassword = "correct-horse-battery"

func TestRunResetsThePasswordAndNeverPrintsIt(t *testing.T) {
	f := &fakeReset{rows: 1}
	var stderr bytes.Buffer
	code := run([]string{"-dsn", "postgres://x", "-email", "ops@example.com",
		"-password", goodPassword}, &stderr, f.deps())
	if code != 0 {
		t.Fatalf("run = %d, want 0; stderr %q", code, stderr.String())
	}
	if f.hashed != goodPassword || f.hash != "hashed:"+goodPassword ||
		f.email != "ops@example.com" {
		t.Fatalf("hashed %q, updated (%q, %q)", f.hashed, f.hash, f.email)
	}
	if f.opened != 1 || f.closed != 1 {
		t.Fatalf("open/close = %d/%d, want 1/1", f.opened, f.closed)
	}
	want := "reset password for ops@example.com (rows=1)\n"
	if stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestRunRefusesUsageErrorsWithExitTwo(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no dsn", []string{"-password", goodPassword}, "dsn and password are required"},
		{"no password", []string{"-dsn", "postgres://x"}, "dsn and password are required"},
		{"no flags", nil, "dsn and password are required"},
		{"11 characters", []string{"-dsn", "postgres://x", "-password", "elevenchars"},
			"password must be at least 12 characters"},
		{"unknown flag", []string{"-dsn", "postgres://x", "-password", goodPassword,
			"-bogus"}, "flag provided but not defined: -bogus"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeReset{rows: 1}
			var stderr bytes.Buffer
			if code := run(tc.args, &stderr, f.deps()); code != 2 {
				t.Fatalf("run = %d, want 2", code)
			}
			if got := strings.TrimSpace(stderr.String()); got != tc.want {
				t.Fatalf("stderr = %q, want %q", got, tc.want)
			}
			if f.opened != 0 {
				t.Fatalf("opened the database %d times on a usage error", f.opened)
			}
			for _, secret := range []string{goodPassword, "elevenchars"} {
				if strings.Contains(stderr.String(), secret) {
					t.Fatalf("stderr leaks the password: %q", stderr.String())
				}
			}
		})
	}
}

func TestRunAcceptsTwelveCharactersAndDefaultsTheEmail(t *testing.T) {
	f := &fakeReset{rows: 1}
	var stderr bytes.Buffer
	code := run([]string{"-dsn", "postgres://x", "-password", "twelve-chars"},
		&stderr, f.deps())
	if code != 0 {
		t.Fatalf("run = %d, want 0; stderr %q", code, stderr.String())
	}
	if f.hashed != "twelve-chars" || f.email != "admin@pg-sage.local" {
		t.Fatalf("hashed %q for %q", f.hashed, f.email)
	}
}

func TestRunReportsEachFailingStepWithExitOne(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name      string
		fake      fakeReset
		want      string
		wantClose int
	}{
		{"open", fakeReset{openErr: boom}, "connect to admin database: boom", 0},
		{"hash", fakeReset{hashErr: boom}, "hash admin password: boom", 1},
		{"update", fakeReset{updateErr: boom}, "update admin password: boom", 1},
		{"no row", fakeReset{rows: 0}, `no user with email "ghost@example.com"`, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.fake
			var stderr bytes.Buffer
			code := run([]string{"-dsn", "postgres://x", "-email", "ghost@example.com",
				"-password", goodPassword}, &stderr, f.deps())
			if code != 1 {
				t.Fatalf("run = %d, want 1", code)
			}
			if got := strings.TrimSpace(stderr.String()); got != tc.want {
				t.Fatalf("stderr = %q, want %q", got, tc.want)
			}
			if f.closed != tc.wantClose {
				t.Fatalf("closed %d times, want %d", f.closed, tc.wantClose)
			}
		})
	}
}

// The production open must reach the server: a pool alone connects lazily,
// so an unreachable DSN used to surface as an update error.
func TestRunWithProductionDepsReportsAnUnreachableServer(t *testing.T) {
	for name, dsn := range map[string]string{
		"refused":   "postgres://sage:pw@127.0.0.1:1/sage?connect_timeout=5&sslmode=disable",
		"malformed": "postgres://sage:pw@127.0.0.1:notaport/sage",
	} {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := run([]string{"-dsn", dsn, "-password", goodPassword}, &stderr,
				productionResetDeps())
			if code != 1 {
				t.Fatalf("run = %d, want 1; stderr %q", code, stderr.String())
			}
			if !strings.HasPrefix(stderr.String(), "connect to admin database: ") {
				t.Fatalf("stderr = %q, want the connect step", stderr.String())
			}
			if strings.Contains(stderr.String(), goodPassword) {
				t.Fatalf("stderr leaks the password: %q", stderr.String())
			}
		})
	}
}
