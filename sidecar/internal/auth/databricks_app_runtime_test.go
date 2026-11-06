package auth

import (
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/appmode"
)

// No concurrent access tests: the loader reads the environment once at
// startup through a caller-supplied getenv and holds no shared state.

func appRuntimeEnv(overrides map[string]string) func(string) string {
	env := map[string]string{
		"DATABRICKS_HOST":             "https://workspace.cloud.databricks.com",
		"SAGE_DATABRICKS_ADMIN_EMAIL": "owner@example.com",
	}
	for k, v := range overrides {
		env[k] = v
	}
	return func(key string) string { return env[key] }
}

func TestDatabricksAppPortRequiresAppAuthMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"port with mode unset", map[string]string{"DATABRICKS_APP_PORT": "8000"}},
		{"port with session mode", map[string]string{
			"DATABRICKS_APP_PORT": "8000", "SAGE_AUTH_MODE": "session",
		}},
		{"port with unknown mode", map[string]string{
			"DATABRICKS_APP_PORT": "8000", "SAGE_AUTH_MODE": "proxy",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadDatabricksAppConfigFromEnv(appRuntimeEnv(tc.env))
			if cfg != nil {
				t.Fatalf("App runtime without App auth returned config %+v", cfg)
			}
			if !errors.Is(err, appmode.ErrAppAuthRequired) {
				t.Fatalf("err = %v, want ErrAppAuthRequired", err)
			}
			if !strings.Contains(err.Error(), "SAGE_AUTH_MODE=databricks_app") ||
				!strings.Contains(err.Error(), "DATABRICKS_APP_PORT") {
				t.Fatalf("error %q must name SAGE_AUTH_MODE and DATABRICKS_APP_PORT", err)
			}
		})
	}
}

func TestDatabricksAppModeKeepsPortValidation(t *testing.T) {
	for _, port := range []string{"0", "abc", "-1", "65536"} {
		t.Run(port, func(t *testing.T) {
			cfg, err := LoadDatabricksAppConfigFromEnv(appRuntimeEnv(map[string]string{
				"DATABRICKS_APP_PORT": port, "SAGE_AUTH_MODE": DatabricksAppMode,
			}))
			if cfg != nil || err == nil {
				t.Fatalf("port %q accepted: %+v, %v", port, cfg, err)
			}
			if errors.Is(err, appmode.ErrAppAuthRequired) ||
				!strings.Contains(err.Error(), "Databricks Apps runtime port") {
				t.Fatalf("port %q gave %v, want the runtime port error", port, err)
			}
		})
	}
	for _, port := range []string{"1", "65535"} {
		cfg, err := LoadDatabricksAppConfigFromEnv(appRuntimeEnv(map[string]string{
			"DATABRICKS_APP_PORT": port, "SAGE_AUTH_MODE": DatabricksAppMode,
		}))
		if err != nil || cfg == nil || cfg.AdminEmail != "owner@example.com" {
			t.Fatalf("boundary port %q = %+v, %v", port, cfg, err)
		}
	}
}

func TestSessionAuthOutsideAppsUnchanged(t *testing.T) {
	for _, mode := range []string{"", "session"} {
		t.Run("mode="+mode, func(t *testing.T) {
			cfg, err := LoadDatabricksAppConfigFromEnv(appRuntimeEnv(map[string]string{
				"SAGE_AUTH_MODE": mode,
			}))
			if err != nil || cfg != nil {
				t.Fatalf("session auth outside Apps = %+v, %v; want nil, nil", cfg, err)
			}
		})
	}
	cfg, err := LoadDatabricksAppConfigFromEnv(func(string) string { return "" })
	if err != nil || cfg != nil {
		t.Fatalf("empty environment = %+v, %v; want nil, nil", cfg, err)
	}
}

// An unknown mode outside Apps must keep its own error now that the port
// check runs first; it must never fall back to session auth.
func TestUnknownAuthModeOutsideAppsRejected(t *testing.T) {
	cfg, err := LoadDatabricksAppConfigFromEnv(appRuntimeEnv(map[string]string{
		"SAGE_AUTH_MODE": "proxy",
	}))
	if cfg != nil || err == nil {
		t.Fatalf("unknown mode outside Apps = %+v, %v; want an error", cfg, err)
	}
	if errors.Is(err, appmode.ErrAppAuthRequired) ||
		!strings.Contains(err.Error(), "SAGE_AUTH_MODE must be session or databricks_app") {
		t.Fatalf("unknown mode outside Apps gave %v, want the mode error", err)
	}
}
