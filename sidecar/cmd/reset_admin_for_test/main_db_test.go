//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// apiauth-14: run with the production dependencies against the real schema.

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "cmd/reset_admin_for_test"))
}

const (
	adminEmail  = "admin@example.com"
	oldPassword = "bootstrap-generated-pw"
)

// seededDB bootstraps the schema and creates the one admin the way a first
// start does with a generated password, so must_change_password is true.
func seededDB(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap the sage schema: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"TRUNCATE sage.users RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("empty sage.users: %v", err)
	}
	err = auth.BootstrapGeneratedAdmin(ctx, pool, adminEmail, oldPassword)
	if err != nil {
		t.Fatalf("bootstrap the admin: %v", err)
	}
	return dsn, pool
}

func runReset(dsn, email, password string) (int, string) {
	var stderr bytes.Buffer
	code := run([]string{"-dsn", dsn, "-email", email, "-password", password},
		&stderr, productionResetDeps())
	return code, stderr.String()
}

func adminRow(t *testing.T, pool *pgxpool.Pool) (string, bool) {
	t.Helper()
	var hash string
	var mustChange bool
	err := pool.QueryRow(context.Background(), `SELECT password, must_change_password
		FROM sage.users WHERE email = $1`, adminEmail).Scan(&hash, &mustChange)
	if err != nil {
		t.Fatalf("read the admin row: %v", err)
	}
	return hash, mustChange
}

// State transitions: generated admin (must change) -> reset -> sign in with
// the new password; the old one no longer works.
func TestRunResetsTheHashAndClearsMustChange(t *testing.T) {
	dsn, pool := seededDB(t)
	before, mustChange := adminRow(t, pool)
	if !mustChange {
		t.Fatal("seed: a generated admin must start with must_change_password")
	}
	code, out := runReset(dsn, adminEmail, goodPassword)
	if code != 0 {
		t.Fatalf("run = %d; stderr %q", code, out)
	}
	if out != "reset password for "+adminEmail+" (rows=1)\n" {
		t.Fatalf("stderr = %q", out)
	}
	after, mustChange := adminRow(t, pool)
	if after == before || mustChange {
		t.Fatalf("hash changed=%v must_change=%v", after != before, mustChange)
	}
	if !auth.CheckPassword(after, goodPassword) || auth.CheckPassword(after, oldPassword) {
		t.Fatal("stored hash does not match only the new password")
	}
	ctx := context.Background()
	user, err := auth.Authenticate(ctx, pool, adminEmail, goodPassword)
	if err != nil || user.Email != adminEmail || user.Role != auth.RoleAdmin {
		t.Fatalf("sign in with the new password = (%+v, %v)", user, err)
	}
	_, err = auth.Authenticate(ctx, pool, adminEmail, oldPassword)
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("sign in with the old password: err = %v, want invalid credentials", err)
	}
}

func TestRunRefusesAnUnknownEmail(t *testing.T) {
	dsn, pool := seededDB(t)
	before, _ := adminRow(t, pool)
	code, out := runReset(dsn, "ghost@example.com", goodPassword)
	if code != 1 || strings.TrimSpace(out) != `no user with email "ghost@example.com"` {
		t.Fatalf("run = %d, stderr %q", code, out)
	}
	if after, _ := adminRow(t, pool); after != before {
		t.Fatal("a refused reset changed the admin's hash")
	}
}

func TestRunReportsAMissingUsersTable(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "no_sage_schema")
	code, out := runReset(dsn, adminEmail, goodPassword)
	if code != 1 || !strings.HasPrefix(out, "update admin password: ") {
		t.Fatalf("run = %d, stderr %q", code, out)
	}
	if strings.Contains(out, goodPassword) {
		t.Fatalf("stderr leaks the password: %q", out)
	}
}

func TestConcurrentResetsLeaveOneOfTheirPasswords(t *testing.T) {
	dsn, pool := seededDB(t)
	passwords := []string{"first-password-1", "second-password-2", "third-password-3"}
	var wg sync.WaitGroup
	codes := make([]int, len(passwords))
	for i, pw := range passwords {
		wg.Add(1)
		go func(i int, pw string) {
			defer wg.Done()
			codes[i], _ = runReset(dsn, adminEmail, pw)
		}(i, pw)
	}
	wg.Wait()
	for i, code := range codes {
		if code != 0 {
			t.Fatalf("reset %d = %d, want 0", i, code)
		}
	}
	hash, mustChange := adminRow(t, pool)
	matches := 0
	for _, pw := range passwords {
		if auth.CheckPassword(hash, pw) {
			matches++
		}
	}
	if matches != 1 || mustChange {
		t.Fatalf("final hash matches %d passwords, must_change=%v", matches, mustChange)
	}
}
