package config

import (
	"strings"
	"testing"
)

// lab-01c: verify.method chooses the verifier (v1, shadow, v2 or auto) and
// verify.on_expired what happens to a monitor whose window ended unwatched
// (ask, keep or revert). Both are plain strings read once at load: no
// shared state, so no concurrent-access test applies.

func TestVerifyMethod_DefaultsWithoutAConfigFile(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Verify.Method != "auto" || cfg.Verify.OnExpired != "ask" {
		t.Fatalf("defaults: method %q on_expired %q, want auto and ask", cfg.Verify.Method,
			cfg.Verify.OnExpired)
	}
	chdirTemp(t)
	loaded, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Verify.Method != "auto" || loaded.Verify.OnExpired != "ask" {
		t.Fatalf("loaded with no file: method %q on_expired %q, want auto and ask",
			loaded.Verify.Method, loaded.Verify.OnExpired)
	}
}

func TestVerifyMethod_PartialVerifySectionKeepsDefaults(t *testing.T) {
	cfg, err := loadRCAYAML(t, "verify:\n  min_samples: 5\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Verify.Method != "auto" || cfg.Verify.OnExpired != "ask" {
		t.Fatalf("partial section masked the defaults: method %q on_expired %q",
			cfg.Verify.Method, cfg.Verify.OnExpired)
	}
}

func TestVerifyMethod_AcceptsEveryAllowedValue(t *testing.T) {
	for _, method := range []string{"v1", "shadow", "v2", "auto"} {
		cfg, err := loadRCAYAML(t, "verify:\n  method: "+method+"\n")
		if err != nil {
			t.Fatalf("method %s refused: %v", method, err)
		}
		if cfg.Verify.Method != method {
			t.Fatalf("method %s loaded as %q", method, cfg.Verify.Method)
		}
	}
	for _, onExpired := range []string{"ask", "keep", "revert"} {
		cfg, err := loadRCAYAML(t, "verify:\n  on_expired: "+onExpired+"\n")
		if err != nil {
			t.Fatalf("on_expired %s refused: %v", onExpired, err)
		}
		if cfg.Verify.OnExpired != onExpired {
			t.Fatalf("on_expired %s loaded as %q", onExpired, cfg.Verify.OnExpired)
		}
	}
}

func TestVerifyMethod_RefusesUnknownValues(t *testing.T) {
	cases := []struct {
		yaml, key string
		allowed   []string
	}{
		{"verify:\n  method: v3\n", "verify.method", []string{"v1", "shadow", "v2", "auto"}},
		{"verify:\n  method: V2\n", "verify.method", []string{"v1", "shadow", "v2", "auto"}},
		{"verify:\n  on_expired: maybe\n", "verify.on_expired",
			[]string{"ask", "keep", "revert"}},
	}
	for _, tc := range cases {
		_, err := loadRCAYAML(t, tc.yaml)
		if err == nil {
			t.Fatalf("%q was accepted", tc.yaml)
		}
		msg := err.Error()
		if !strings.Contains(msg, tc.key) {
			t.Fatalf("error %q does not name %s", msg, tc.key)
		}
		for _, v := range tc.allowed {
			if !strings.Contains(msg, v) {
				t.Fatalf("error %q does not list the allowed value %q", msg, v)
			}
		}
	}
}

// An explicit empty string is not a silent default: it is refused like any
// other unknown value, so a typo'd template cannot switch the verifier.
func TestVerifyMethod_RefusesEmptyValues(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Verify.Method = ""
	if err := cfg.Verify.validateMethod(); err == nil ||
		!strings.Contains(err.Error(), "verify.method") {
		t.Fatalf("empty method: %v, want a refusal naming verify.method", err)
	}
	cfg = DefaultConfig()
	cfg.Verify.OnExpired = ""
	if err := cfg.Verify.validateMethod(); err == nil ||
		!strings.Contains(err.Error(), "verify.on_expired") {
		t.Fatalf("empty on_expired: %v, want a refusal naming verify.on_expired", err)
	}
}
