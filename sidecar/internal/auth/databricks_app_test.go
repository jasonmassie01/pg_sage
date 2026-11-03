package auth

import (
	"context"
	"errors"
	"reflect"
	"strings"
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

func TestDatabricksAppAudienceConfig(t *testing.T) {
	base := map[string]string{
		"SAGE_AUTH_MODE": DatabricksAppMode, "DATABRICKS_APP_PORT": "8000",
		"DATABRICKS_HOST": "workspace.cloud.databricks.com", "SAGE_DATABRICKS_ADMIN_EMAIL": "Owner@Example.com",
	}
	load := func(raw string) (*DatabricksAppConfig, error) {
		return LoadDatabricksAppConfigFromEnv(func(key string) string {
			if key == "SAGE_DATABRICKS_ALLOWED_EMAIL_DOMAINS" {
				return raw
			}
			return base[key]
		})
	}
	cfg, err := load(" EXAMPLE.com,other.example.com,example.com ")
	if err != nil || !reflect.DeepEqual(cfg.AllowedEmailDomains, []string{"example.com", "other.example.com"}) {
		t.Fatalf("normalized policy = %+v, %v", cfg, err)
	}
	for _, raw := range []string{
		" ", ",", "example.com,", ",example.com", "example.com,,other.example.com", "*.example.com",
		"https://example.com", "example.com/path", "example.com:443", "user@example.com", ".example.com",
		"example.com.", "example..com", "-bad.example.com", "bad-.example.com", "bad_domain.example.com",
		"exam ple.com", "localhost", "127.0.0.1", "[::1]", "exämple.com", strings.Repeat("a", 64) + ".com",
		strings.Repeat("a.", 126) + "com", "other.example.com",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := load(raw); err == nil {
				t.Fatal("invalid policy or locked-out administrator accepted")
			}
		})
	}
	cfg, err = load("")
	if err != nil || len(cfg.AllowedEmailDomains) != 0 {
		t.Fatalf("omitted policy = %+v, %v", cfg, err)
	}
	// Standalone authentication does not adopt App-specific audience policy.
	_, err = LoadDatabricksAppConfigFromEnv(func(key string) string {
		if key == "SAGE_AUTH_MODE" {
			return "session"
		}
		return "invalid policy"
	})
	if err != nil {
		t.Fatalf("App policy affected standalone: %v", err)
	}
}

func TestDatabricksAppAudienceExactDomainBeforeAccountStore(t *testing.T) {
	cfg := DatabricksAppConfig{AllowedEmailDomains: []string{"example.com"}, AdminEmail: "old-admin@external.com"}
	for _, email := range []string{"viewer@example.com", "Viewer@EXAMPLE.COM"} {
		if err := cfg.CheckEmailAudience(email); err != nil {
			t.Errorf("allowed identity %q: %v", email, err)
		}
	}
	for _, email := range []string{
		"viewer@sub.example.com", "viewer@notexample.com", "viewer@example.com.evil.test",
		"viewer@example-com.test", cfg.AdminEmail,
	} {
		t.Run(email, func(t *testing.T) {
			_, err := ResolveDatabricksAppUser(context.Background(), nil, cfg, email)
			if !errors.Is(err, ErrDatabricksAppAudienceDenied) {
				t.Fatalf("denied identity reached nil account store: %v", err)
			}
		})
	}
	if err := BootstrapDatabricksAppAdmin(context.Background(), nil, cfg); !errors.Is(err, ErrDatabricksAppAudienceDenied) {
		t.Fatalf("bootstrap bypassed audience before account store: %v", err)
	}
	if err := (DatabricksAppConfig{}).CheckEmailAudience("viewer@external.com"); err != nil {
		t.Fatalf("omitted policy changed default audience: %v", err)
	}
}
