package config

import (
	"strings"
	"testing"
)

// DescribeField explains one configuration key for Ask Sage's
// explain_config tool: its documentation, lifecycle and current value.
// Values that are or may carry a credential are never returned.

func TestDescribeField_LeafWithValue(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Ask.RetentionDays = 12
	got, ok := DescribeField(cfg, "ask.retention_days")
	if !ok {
		t.Fatal("ask.retention_days not described")
	}
	if got.Path != "ask.retention_days" || !strings.Contains(got.Doc, "Default: 30") ||
		got.Lifecycle != string(LifecycleRestart) || got.Secret {
		t.Fatalf("doc = %+v", got)
	}
	if v, ok := got.Value.(int); !ok || v != 12 {
		t.Fatalf("value = %#v, want the current 12", got.Value)
	}
	if !got.HasValue {
		t.Fatal("a plain integer must report its value")
	}
}

func TestDescribeField_NilConfigUsesDefaults(t *testing.T) {
	got, ok := DescribeField(nil, "ask.enabled")
	if !ok || got.Value != true || !got.HasValue {
		t.Fatalf("nil config = %+v (%v)", got, ok)
	}
}

func TestDescribeField_NeverReturnsCredentials(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LLM.APIKey = "sk-should-never-leak"
	cfg.Postgres.Password = "hunter2"
	cfg.Postgres.DatabaseURL = "postgres://u:hunter2@h/db"
	cfg.MetaDB = "postgres://u:hunter2@meta/db"
	cfg.Databases = []DatabaseConfig{{Name: "a", Password: "hunter2"}}
	for _, key := range []string{"llm.api_key", "postgres.password", "postgres.database_url",
		"meta_db", "databases", "encryption_key", "llm.endpoint"} {
		got, ok := DescribeField(cfg, key)
		if !ok {
			t.Errorf("%s not described", key)
			continue
		}
		if got.HasValue || got.Value != nil {
			t.Errorf("%s returned value %#v", key, got.Value)
		}
		if strings.Contains(got.Doc, "hunter2") || strings.Contains(got.Doc, "sk-should") {
			t.Errorf("%s doc leaks a secret: %q", key, got.Doc)
		}
	}
	if got, _ := DescribeField(cfg, "llm.api_key"); !got.Secret {
		t.Error("llm.api_key is not marked secret")
	}
}

func TestDescribeField_UnknownAndNonLeafKeys(t *testing.T) {
	for _, key := range []string{"", "ask", "nope.x", "ask.retention_days.x", "ASK.ENABLED",
		"llm", " ask.enabled"} {
		if got, ok := DescribeField(DefaultConfig(), key); ok {
			t.Errorf("%q described as %+v", key, got)
		}
	}
}

func TestDescribeField_SliceOfScalarsKeepsItsValue(t *testing.T) {
	cfg := DefaultConfig()
	cfg.API.TrustedProxies = []string{"10.0.0.0/8"}
	got, ok := DescribeField(cfg, "api.trusted_proxies")
	if !ok {
		t.Fatal("api.trusted_proxies not described")
	}
	list, isList := got.Value.([]string)
	if !got.HasValue || !isList || len(list) != 1 || list[0] != "10.0.0.0/8" {
		t.Fatalf("value = %#v", got.Value)
	}
}
