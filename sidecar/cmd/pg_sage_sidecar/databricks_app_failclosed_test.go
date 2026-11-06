package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/appmode"
)

// No concurrent access tests: the Apps check is read once at startup from
// the process environment.

// assertNoPasswordPrinted fails when stderr carries either generated-password
// banner (standalone or meta-db startup).
func assertNoPasswordPrinted(t *testing.T, stderr string) {
	t.Helper()
	for _, banner := range []string{"INITIAL ADMIN PASSWORD", "Initial password"} {
		if strings.Contains(stderr, banner) {
			t.Fatalf("stderr printed %q inside Databricks Apps:\n%s",
				banner, redactAdminPassword(stderr))
		}
	}
}

func TestDatabricksAppPortWithoutAuthModeRefusesBootstrap(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(context.Context, *pgxpool.Pool) error
	}{
		{"standalone", bootstrapAdminIfEmpty}, {"metadb", bootstrapAdminUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, p, ctx := firstLookDB(t)
			clearBootstrapAdminEnv(t)
			t.Setenv("SAGE_AUTH_MODE", "")
			t.Setenv("DATABRICKS_APP_PORT", "8000")
			var err error
			stderr := captureStderr(t, func() { err = tc.run(ctx, p) })
			if !errors.Is(err, appmode.ErrAppAuthRequired) ||
				!strings.Contains(err.Error(), "SAGE_AUTH_MODE") {
				t.Fatalf("bootstrap err = %v, want ErrAppAuthRequired naming SAGE_AUTH_MODE", err)
			}
			assertNoPasswordPrinted(t, stderr)
			var users int
			if err := p.QueryRow(ctx, "SELECT count(*) FROM sage.users").Scan(&users); err != nil {
				t.Fatalf("count users: %v", err)
			}
			if users != 0 {
				t.Fatalf("bootstrap created %d user rows inside Apps without App auth", users)
			}
		})
	}
}

func TestDatabricksAppGeneratedPasswordRefusedInApps(t *testing.T) {
	for _, env := range []map[string]string{
		{"DATABRICKS_APP_PORT": "8000", "SAGE_AUTH_MODE": ""},
		{"DATABRICKS_APP_PORT": "", "SAGE_AUTH_MODE": appmode.AuthMode},
	} {
		clearBootstrapAdminEnv(t)
		for k, v := range env {
			t.Setenv(k, v)
		}
		creds, err := loadBootstrapAdminCredentials()
		if err == nil || creds.password != "" {
			t.Fatalf("env %v generated a printable password: %v", env, err)
		}
		if !strings.Contains(err.Error(), "SAGE_AUTH_MODE=databricks_app") {
			t.Fatalf("env %v: error %q does not name the fix", env, err)
		}
	}
}

func TestDatabricksAppGeneratedPasswordOutsideAppsUnchanged(t *testing.T) {
	clearBootstrapAdminEnv(t)
	t.Setenv("DATABRICKS_APP_PORT", "")
	t.Setenv("SAGE_AUTH_MODE", "session")
	creds, err := loadBootstrapAdminCredentials()
	if err != nil || creds.supplied || creds.email != adminEmail ||
		len(creds.password) != adminPassLen {
		t.Fatalf("session auth outside Apps lost generated credentials: %+v, %v",
			creds.email, err)
	}
}

func TestMainChar_DatabricksAppPortWithoutAuthModeExitsOne(t *testing.T) {
	path := writeChildConfig(t, "mode: standalone\n")
	child := startMainChild(t, []string{"DATABRICKS_APP_PORT=8000", "SAGE_AUTH_MODE="},
		"--config", path)
	if code := child.wait(t, 60*time.Second); code != 1 {
		t.Fatalf("App port without App auth exit = %d, output %q", code,
			redactAdminPassword(child.out.String()))
	}
	out := child.out.String()
	want := "[ERROR] [startup] authentication configuration: running inside Databricks Apps"
	if !strings.Contains(out, want) || !strings.Contains(out, "SAGE_AUTH_MODE=databricks_app") {
		t.Fatalf("output does not explain the fail-closed rule: %q", out)
	}
	if strings.Contains(out, "connected") {
		t.Fatalf("sidecar connected before refusing: %q", out)
	}
	assertNoPasswordPrinted(t, out)
}
