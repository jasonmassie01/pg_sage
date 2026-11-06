package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// apiauth-10: api.trusted_origins lists the browser origins whose
// state-changing requests the API accepts although they are cross-origin.

func TestTrustedOrigins_DefaultIsEmpty(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.API.TrustedOrigins) != 0 {
		t.Fatalf("default trusted_origins = %#v, want empty", cfg.API.TrustedOrigins)
	}
	if err := cfg.API.validateTrustedOrigins(); err != nil {
		t.Fatalf("empty trusted_origins must validate: %v", err)
	}
}

func TestTrustedOrigins_ValidEntriesAccepted(t *testing.T) {
	api := APIConfig{TrustedOrigins: []string{
		"https://ui.example", "http://localhost:5173", "https://ui.example:8443",
		"http://[::1]:8080",
	}}
	if err := api.validateTrustedOrigins(); err != nil {
		t.Fatalf("valid origins refused: %v", err)
	}
}

func TestTrustedOrigins_MalformedEntryNamesTheKey(t *testing.T) {
	for _, bad := range []string{
		"", " ", "ui.example", "https://", "ftp://ui.example", "https://ui.example/",
		"https://ui.example/app", "https://ui.example?x=1", "https://ui.example#f",
		"https://user:pw@ui.example", "javascript:alert(1)", "https://ui example",
		"*", "null", "https://UI.example", "https://ui.example:443", "http://ui.example:80",
	} {
		api := APIConfig{TrustedOrigins: []string{"https://ok.example", bad}}
		err := api.validateTrustedOrigins()
		if err == nil {
			t.Errorf("%q accepted as a trusted origin", bad)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "api.trusted_origins[1]") {
			t.Errorf("%q: error %q does not name api.trusted_origins[1]", bad, msg)
		}
		if !strings.Contains(msg, "scheme://host[:port]") {
			t.Errorf("%q: error %q does not say the expected form", bad, msg)
		}
	}
}

func TestTrustedOrigins_LoadRejectsMalformedYAML(t *testing.T) {
	chdirTemp(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "api:\n  trusted_origins:\n    - https://ui.example/path\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load([]string{"--config", path})
	if err == nil || !strings.Contains(err.Error(), "api.trusted_origins[0]") {
		t.Fatalf("Load error = %v, want one naming api.trusted_origins[0]", err)
	}
}

func TestTrustedOrigins_FromYAMLAndEnv(t *testing.T) {
	chdirTemp(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "api:\n  trusted_origins:\n    - https://ui.example\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load([]string{"--config", path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(cfg.API.TrustedOrigins, []string{"https://ui.example"}) {
		t.Fatalf("yaml trusted_origins = %#v", cfg.API.TrustedOrigins)
	}

	t.Setenv("SAGE_API_TRUSTED_ORIGINS", " https://a.example , ,https://b.example:8443 ")
	cfg, err = Load([]string{"--config", path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"https://a.example", "https://b.example:8443"}
	if !reflect.DeepEqual(cfg.API.TrustedOrigins, want) {
		t.Fatalf("env must replace yaml: got %#v, want %#v", cfg.API.TrustedOrigins, want)
	}
}

func TestTrustedOrigins_EnvMalformedFailsLoad(t *testing.T) {
	chdirTemp(t)
	t.Setenv("SAGE_API_TRUSTED_ORIGINS", "https://a.example,ui.example")
	_, err := Load(nil)
	if err == nil || !strings.Contains(err.Error(), "api.trusted_origins[1]") {
		t.Fatalf("Load error = %v, want one naming api.trusted_origins[1]", err)
	}
}

func TestTrustedOrigins_EnvEmptyKeepsYAML(t *testing.T) {
	chdirTemp(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "api:\n  trusted_origins: [https://ui.example]\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SAGE_API_TRUSTED_ORIGINS", "")
	cfg, err := Load([]string{"--config", path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(cfg.API.TrustedOrigins, []string{"https://ui.example"}) {
		t.Fatalf("unset env must keep yaml: %#v", cfg.API.TrustedOrigins)
	}
}

func TestTrustedOrigins_RegistryLifecycleAndClone(t *testing.T) {
	lifecycle, ok := LookupFieldLifecycle("api.trusted_origins")
	if !ok || lifecycle.Lifecycle != LifecycleRestart {
		t.Fatalf("api.trusted_origins lifecycle = %+v, want restart", lifecycle)
	}
	if class, ok := KeyClassOf("api.trusted_origins"); !ok || class != KeySafetyCritical {
		t.Fatalf("api.trusted_origins class = %q, want safety_critical", class)
	}
	cfg := DefaultConfig()
	cfg.API.TrustedOrigins = []string{"https://ui.example"}
	cp := Clone(cfg)
	cp.API.TrustedOrigins[0] = "https://changed.example"
	if cfg.API.TrustedOrigins[0] != "https://ui.example" {
		t.Fatal("Clone shares the trusted_origins backing array")
	}
	desc, ok := DescribeField(cfg, "api.trusted_origins")
	if !ok || desc.Doc == "" {
		t.Fatalf("api.trusted_origins not documented: %+v", desc)
	}
}

// No concurrent-access test: validation and the env overlay are pure
// functions of one Config value with no shared state.

// A browser never sends an empty port, so "https://ui.example:" could never
// match and is refused like any other malformed entry.
func TestTrustedOrigins_EmptyPortRefused(t *testing.T) {
	api := APIConfig{TrustedOrigins: []string{"https://ui.example:"}}
	err := api.validateTrustedOrigins()
	if err == nil || !strings.Contains(err.Error(), "port is empty") ||
		!strings.Contains(err.Error(), "api.trusted_origins[0]") {
		t.Fatalf("empty port: err = %v, want a named port-is-empty error", err)
	}
}
