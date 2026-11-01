package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

func clearBootstrapAdminEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"SAGE_BOOTSTRAP_ADMIN_EMAIL", "SAGE_BOOTSTRAP_ADMIN_PASSWORD", "SAGE_BOOTSTRAP_ADMIN_PASSWORD_FILE"} {
		t.Setenv(name, "")
	}
}

func TestBootstrapAdminCredentialsDefault(t *testing.T) {
	clearBootstrapAdminEnv(t)
	first, err := loadBootstrapAdminCredentials()
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadBootstrapAdminCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if first.email != adminEmail || len(first.password) != adminPassLen || first.supplied || first.password == second.password {
		t.Fatal("expected fresh random credentials with the default local email")
	}
}

func TestBootstrapAdminCredentialsExplicit(t *testing.T) {
	for _, source := range []string{"env", "file"} {
		t.Run(source, func(t *testing.T) {
			clearBootstrapAdminEnv(t)
			t.Setenv("SAGE_BOOTSTRAP_ADMIN_EMAIL", "owner@example.com")
			const password = "secret-test-bootstrap-password"
			if source == "env" {
				t.Setenv("SAGE_BOOTSTRAP_ADMIN_PASSWORD", password)
			} else {
				file := filepath.Join(t.TempDir(), "password")
				if err := os.WriteFile(file, []byte(password+"\r\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("SAGE_BOOTSTRAP_ADMIN_PASSWORD_FILE", file)
			}
			credentials, err := loadBootstrapAdminCredentials()
			if err != nil {
				t.Fatal(err)
			}
			if !credentials.supplied || credentials.email != "owner@example.com" || credentials.password != password {
				t.Fatal("explicit credentials did not match the configured source")
			}
		})
	}
}

func TestBootstrapAdminCredentialsRejectInvalidSafely(t *testing.T) {
	const password = "sensitive-bootstrap-password"
	cases := []struct{ name, email, password, file string }{
		{"no-email", "", password, ""},
		{"display-name", "Owner <owner@example.com>", password, ""},
		{"newline", "owner@example.com\n", password, ""},
		{"no-password", "owner@example.com", "", ""},
		{"both-sources", "owner@example.com", password, "/not-read"},
		{"short-password", "owner@example.com", "short", ""},
		{"bcrypt-limit", "owner@example.com", strings.Repeat("x", 73), ""},
		{"missing-file", "owner@example.com", "", "/file-does-not-exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearBootstrapAdminEnv(t)
			t.Setenv("SAGE_BOOTSTRAP_ADMIN_EMAIL", tc.email)
			t.Setenv("SAGE_BOOTSTRAP_ADMIN_PASSWORD", tc.password)
			t.Setenv("SAGE_BOOTSTRAP_ADMIN_PASSWORD_FILE", tc.file)
			_, err := loadBootstrapAdminCredentials()
			if err == nil {
				t.Fatal("expected validation error")
			}
			if tc.password != "" && strings.Contains(err.Error(), tc.password) {
				t.Fatal("validation error exposed supplied password")
			}
		})
	}
}

func TestBootstrapAdminCredentialsRejectInsecureFiles(t *testing.T) {
	clearBootstrapAdminEnv(t)
	t.Setenv("SAGE_BOOTSTRAP_ADMIN_EMAIL", "owner@example.com")
	file := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(file, []byte("sensitive-bootstrap-password"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, t.TempDir()} {
		t.Setenv("SAGE_BOOTSTRAP_ADMIN_PASSWORD_FILE", path)
		if _, err := loadBootstrapAdminCredentials(); err == nil {
			t.Fatal("expected insecure or non-regular password file to be rejected")
		}
	}
}

func TestBootstrapAdminSecretCreatesOnceWithoutLogging(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(context.Context, *pgxpool.Pool) error
	}{
		{"standalone", bootstrapAdminIfEmpty},
		{"metadb", bootstrapAdminUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearBootstrapAdminEnv(t)
			_, p, ctx := firstLookDB(t)
			t.Setenv("SAGE_BOOTSTRAP_ADMIN_EMAIL", "owner@example.com")
			const password = "secret-bootstrap-acceptance-password"
			t.Setenv("SAGE_BOOTSTRAP_ADMIN_PASSWORD", password)
			capture, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			defer capture.Close()
			stderr := os.Stderr
			os.Stderr = capture
			err = tc.run(ctx, p)
			os.Stderr = stderr
			if err != nil {
				t.Fatal(err)
			}
			output, err := os.ReadFile(capture.Name())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(output), password) {
				t.Fatal("supplied password appeared in startup stderr")
			}
			user, err := auth.Authenticate(ctx, p, "owner@example.com", password)
			if err != nil || user.Role != "admin" {
				t.Fatal("supplied credentials did not authenticate as admin")
			}
			// Existing accounts must survive changed/missing bootstrap secrets.
			t.Setenv("SAGE_BOOTSTRAP_ADMIN_EMAIL", "invalid-email")
			t.Setenv("SAGE_BOOTSTRAP_ADMIN_PASSWORD", "")
			t.Setenv("SAGE_BOOTSTRAP_ADMIN_PASSWORD_FILE", "/not-read-on-restart")
			if err := tc.run(ctx, p); err != nil {
				t.Fatalf("existing accounts must bypass bootstrap credentials: %v", err)
			}
			again, err := auth.Authenticate(ctx, p, "owner@example.com", password)
			if err != nil || again.ID != user.ID {
				t.Fatal("existing admin changed during a second bootstrap")
			}
			if count, err := auth.UserCount(ctx, p); err != nil || count != 1 {
				t.Fatal("bootstrap must create exactly one account")
			}
		})
	}
}
