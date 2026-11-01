package auth

import (
	"reflect"
	"testing"
)

func TestDatabricksAppConfigRequiresExplicitTrustedRuntime(t *testing.T) {
	base := map[string]string{
		"SAGE_AUTH_MODE": DatabricksAppMode, "DATABRICKS_APP_PORT": "8000",
		"DATABRICKS_HOST":             "https://workspace.cloud.databricks.com/",
		"SAGE_DATABRICKS_ADMIN_EMAIL": "Owner@example.com",
	}
	for _, tc := range []struct{ name, key, value string }{
		{"unknown mode", "SAGE_AUTH_MODE", "proxy"},
		{"missing runtime", "DATABRICKS_APP_PORT", ""},
		{"invalid port", "DATABRICKS_APP_PORT", "65536"},
		{"missing workspace", "DATABRICKS_HOST", ""},
		{"insecure workspace", "DATABRICKS_HOST", "http://workspace.cloud.databricks.com"},
		{"workspace path", "DATABRICKS_HOST", "https://workspace.cloud.databricks.com/api"},
		{"workspace credentials", "DATABRICKS_HOST", "https://someone@workspace.cloud.databricks.com"},
		{"bare workspace path", "DATABRICKS_HOST", "workspace.cloud.databricks.com/api"},
		{"bare workspace credentials", "DATABRICKS_HOST", "someone@workspace.cloud.databricks.com"},
		{"workspace query", "DATABRICKS_HOST", "workspace.cloud.databricks.com?test=1"},
		{"workspace empty query", "DATABRICKS_HOST", "workspace.cloud.databricks.com?"},
		{"workspace fragment", "DATABRICKS_HOST", "workspace.cloud.databricks.com#test"},
		{"missing owner", "SAGE_DATABRICKS_ADMIN_EMAIL", ""},
		{"ambiguous owner", "SAGE_DATABRICKS_ADMIN_EMAIL", "one@example.com,two@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadDatabricksAppConfigFromEnv(func(key string) string {
				if key == tc.key {
					return tc.value
				}
				return base[key]
			})
			if err == nil {
				t.Fatal("unsafe authentication configuration accepted")
			}
		})
	}
	cfg, err := LoadDatabricksAppConfigFromEnv(func(key string) string { return base[key] })
	if err != nil || cfg.AdminEmail != "owner@example.com" || cfg.Workspace != "https://workspace.cloud.databricks.com" {
		t.Fatalf("valid App configuration = %+v, %v", cfg, err)
	}
	for _, host := range []string{"workspace.cloud.databricks.com", "workspace.cloud.databricks.com/", "https://workspace.cloud.databricks.com"} {
		t.Run(host, func(t *testing.T) {
			normalized, err := LoadDatabricksAppConfigFromEnv(func(key string) string {
				if key == "DATABRICKS_HOST" {
					return host
				}
				return base[key]
			})
			if err != nil || normalized.Workspace != cfg.Workspace || !reflect.DeepEqual(normalized.Identity(normalized.AdminEmail), cfg.Identity(cfg.AdminEmail)) {
				t.Fatalf("platform hostname must retain the same identity: %+v, %v", normalized, err)
			}
		})
	}
	for _, mode := range []string{"", "session"} {
		cfg, err := LoadDatabricksAppConfigFromEnv(func(key string) string {
			if key == "SAGE_AUTH_MODE" {
				return mode
			}
			return base[key]
		})
		if err != nil || cfg != nil {
			t.Fatalf("ordinary mode = %+v, %v", cfg, err)
		}
	}
}
