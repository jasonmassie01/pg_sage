package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/sagepool"
)

// apiauth-14: run is main's body; these tests pin its exit codes and messages.
// No concurrent access here: run keeps no shared state; main_db_test.go runs
// concurrent creates against a real database.

type fakeAdmin struct {
	openErr, resetErr, createErr error
	opened, closed, resets       int
	email, password, role        string
}

func (f *fakeAdmin) deps() adminDeps {
	return adminDeps{
		open: func(context.Context, string) (*pgxpool.Pool, error) {
			f.opened++
			return nil, f.openErr
		},
		close: func(*pgxpool.Pool) { f.closed++ },
		resetSequence: func(context.Context, *pgxpool.Pool) error {
			f.resets++
			return f.resetErr
		},
		createUser: func(
			_ context.Context, _ *pgxpool.Pool, email, password, role string,
		) (int, error) {
			f.email, f.password, f.role = email, password, role
			if f.createErr != nil {
				return 0, f.createErr
			}
			return 42, nil
		},
	}
}

const goodPassword = "correct-horse-battery"

func adminEnv(dsn, email, password string) func(string) string {
	return mapEnv(map[string]string{
		"PG_SAGE_DSN": dsn, "PG_SAGE_ADMIN_EMAIL": email,
		"PG_SAGE_ADMIN_PASSWORD": password,
	})
}

func TestRunCreatesTheAdminAndNeverPrintsThePassword(t *testing.T) {
	f := &fakeAdmin{}
	var stderr bytes.Buffer
	code := run(adminEnv("postgres://x", "ops@example.com", goodPassword), &stderr, f.deps())
	if code != 0 {
		t.Fatalf("run = %d, want 0; stderr %q", code, stderr.String())
	}
	if f.email != "ops@example.com" || f.password != goodPassword || f.role != "admin" {
		t.Fatalf("createUser got (%q, %q, %q)", f.email, f.password, f.role)
	}
	if f.opened != 1 || f.resets != 1 || f.closed != 1 {
		t.Fatalf("open/reset/close = %d/%d/%d, want 1/1/1", f.opened, f.resets, f.closed)
	}
	out := stderr.String()
	for _, want := range []string{
		"Created admin user id=42", "email: ops@example.com",
		"password: [set via PG_SAGE_ADMIN_PASSWORD]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr %q lacks %q", out, want)
		}
	}
	if strings.Contains(out, goodPassword) {
		t.Fatalf("stderr leaks the password: %q", out)
	}
}

func TestRunRefusesABadEnvironmentWithExitOne(t *testing.T) {
	tests := []struct {
		name, dsn, password, want string
	}{
		{"no dsn", "", goodPassword, "PG_SAGE_DSN environment variable must be set"},
		{"no password", "postgres://x", "",
			"PG_SAGE_ADMIN_PASSWORD environment variable must be set"},
		{"11 characters", "postgres://x", "elevenchars",
			"PG_SAGE_ADMIN_PASSWORD must be at least 12 characters"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeAdmin{}
			var stderr bytes.Buffer
			code := run(adminEnv(tc.dsn, "", tc.password), &stderr, f.deps())
			if code != 1 {
				t.Fatalf("run = %d, want 1", code)
			}
			if got := strings.TrimSpace(stderr.String()); got != tc.want {
				t.Fatalf("stderr = %q, want %q", got, tc.want)
			}
			if f.opened != 0 {
				t.Fatalf("opened the database %d times on a config error", f.opened)
			}
			if tc.password != "" && strings.Contains(stderr.String(), tc.password) {
				t.Fatalf("stderr leaks the password: %q", stderr.String())
			}
		})
	}
}

func TestRunAcceptsTwelveCharacterPasswords(t *testing.T) {
	f := &fakeAdmin{}
	var stderr bytes.Buffer
	if code := run(adminEnv("postgres://x", "", "twelve-chars"), &stderr, f.deps()); code != 0 {
		t.Fatalf("run = %d, want 0; stderr %q", code, stderr.String())
	}
	if f.password != "twelve-chars" {
		t.Fatalf("createUser password = %q", f.password)
	}
}

func TestRunDefaultsTheAdminEmail(t *testing.T) {
	f := &fakeAdmin{}
	var stderr bytes.Buffer
	if code := run(adminEnv("postgres://x", "", goodPassword), &stderr, f.deps()); code != 0 {
		t.Fatalf("run = %d, want 0", code)
	}
	if f.email != "admin@pg-sage.local" {
		t.Fatalf("createUser email = %q, want admin@pg-sage.local", f.email)
	}
	if !strings.Contains(stderr.String(), "email: admin@pg-sage.local") {
		t.Fatalf("stderr %q does not name the default email", stderr.String())
	}
}

func TestRunReportsEachFailingStepWithExitOne(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name      string
		fake      fakeAdmin
		want      string
		wantClose int
	}{
		{"open", fakeAdmin{openErr: boom}, "connect to admin database: boom", 0},
		{"sequence", fakeAdmin{resetErr: boom}, "reset sage.users sequence: boom", 1},
		{"insert", fakeAdmin{createErr: boom}, "create admin user: boom", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.fake
			var stderr bytes.Buffer
			code := run(adminEnv("postgres://x", "", goodPassword), &stderr, f.deps())
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
// so an unreachable DSN used to surface as a sequence error.
func TestRunWithProductionDepsReportsAnUnreachableServer(t *testing.T) {
	for name, dsn := range map[string]string{
		"refused":   "postgres://sage:pw@127.0.0.1:1/sage?connect_timeout=5&sslmode=disable",
		"malformed": "postgres://sage:pw@127.0.0.1:notaport/sage",
	} {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := run(adminEnv(dsn, "", goodPassword), &stderr, productionAdminDeps())
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

// adminHalfOne and adminHalfTwo are the pieces of a DSN password split at
// '@', ':' or '/'. pgx's own parse error printed one of them: its redaction
// keeps the piece after '@' or before ':', and the url error it wraps
// quotes the piece before '/' as an invalid port.
const adminHalfOne, adminHalfTwo = "Admin-Half-One", "Admin-Half-Two"

// Invalid input: a DSN that does not parse fails openAdminPool with
// sagepool.ErrUnparseableDSN and run's connect step with its fixed text,
// and neither carries any piece of the DSN's password.
func TestRunWithProductionDepsNeverPrintsAnUnparseableDSN(t *testing.T) {
	want := "connect to admin database: " + sagepool.ErrUnparseableDSN.Error() + "\n"
	split := func(sep, port string) string {
		return "postgres://sage:" + adminHalfOne + sep + adminHalfTwo + "@127.0.0.1:" + port +
			"/sage"
	}
	for name, dsn := range map[string]string{
		"port":          "postgres://sage:" + adminHalfOne + "@127.0.0.1:notaport/sage",
		"@ in password": split("@", "notaport"),
		": in password": split(":", "notaport"),
		"/ in password": split("/", "5432"),
	} {
		t.Run(name, func(t *testing.T) {
			pool, err := openAdminPool(context.Background(), dsn)
			if pool != nil {
				pool.Close()
			}
			if pool != nil || !errors.Is(err, sagepool.ErrUnparseableDSN) {
				t.Errorf("openAdminPool = pool %v, %v; want none and sagepool.ErrUnparseableDSN",
					pool != nil, err)
			}
			var stderr bytes.Buffer
			code := run(adminEnv(dsn, "", goodPassword), &stderr, productionAdminDeps())
			if code != 1 || stderr.String() != want {
				t.Errorf("run = %d, stderr %q; want 1 and %q", code, stderr.String(), want)
			}
			printed := fmt.Sprint(err) + stderr.String()
			for _, piece := range []string{adminHalfOne, adminHalfTwo, goodPassword} {
				if strings.Contains(printed, piece) {
					t.Errorf("output carries %q: %q", piece, printed)
				}
			}
		})
	}
}

// Without a reachable server the sequence step fails before it holds a
// session, says which step failed, and close still closes the pool.
func TestProductionSequenceStepWithoutAServerClosesCleanly(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx,
		"postgres://sage:pw@127.0.0.1:1/sage?connect_timeout=5&sslmode=disable")
	if err != nil {
		t.Fatalf("lazy pool: %v", err)
	}
	deps := productionAdminDeps()
	err = deps.resetSequence(ctx, pool)
	if err == nil || !strings.HasPrefix(err.Error(),
		"acquire a session for the sequence lock: ") {
		t.Fatalf("resetSequence = %v, want the acquire step", err)
	}
	deps.close(pool)
	if _, err := pool.Acquire(ctx); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Acquire after close = %v, want a closed pool", err)
	}
}
