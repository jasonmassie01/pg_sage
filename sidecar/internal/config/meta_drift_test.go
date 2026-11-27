package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// tier1Fields lists the v0.8.5 Tier 1 config keys per
// reviews/archive/docs-internal/plan_v0.8.5.md §7.3.4. Every key here must have a non-empty
// doc string in config_meta.json and match the live config tags.
//
// Tier 2 coverage is verified by a non-fatal log line so that future
// releases can progressively extend the tagged surface without
// breaking the build on day one.
var tier1Fields = []string{
	// trust.*
	"trust.level",
	"trust.ramp_start",
	"trust.maintenance_window",
	"trust.tier3_safe",
	"trust.tier3_moderate",
	"trust.tier3_high_risk",
	"trust.rollback_threshold_pct",
	"trust.rollback_window_minutes",
	"trust.rollback_cooldown_days",
	"trust.cascade_cooldown_cycles",
	// safety.*
	"safety.query_timeout_ms",
	"safety.lock_timeout_ms",
	"safety.ddl_timeout_seconds",
	"safety.cpu_ceiling_pct",
	"safety.backoff_consecutive_skips",
	// tuner.*
	"tuner.verify_after_apply",
}

// tier2Fields are fields that SHOULD have documentation per plan
// §7.3.4 but are not yet fatal if missing. Logged as warnings.
var tier2Fields = []string{
	"tuner.enabled",
	"tuner.llm_enabled",
	"tuner.work_mem_max_mb",
	"tuner.hint_retirement_days",
	"tuner.revalidation_interval_hours",
	"tuner.stale_stats_estimate_skew",
	"tuner.stale_stats_mod_ratio",
	"tuner.analyze_max_table_mb",
	"tuner.analyze_timeout_ms",
	"analyzer.unused_index_window_days",
	"analyzer.table_bloat_dead_tuple_pct",
	"analyzer.table_bloat_min_rows",
	"analyzer.xid_wraparound_warning",
	"analyzer.xid_wraparound_critical",
	"analyzer.work_mem_promotion_threshold",
	"llm.enabled",
	"llm.endpoint",
	"llm.api_key",
	"llm.model",
	"llm.timeout_seconds",
}

// TestTier1FieldsHaveDocTags — CHECK-T01/T09: every Tier 1 key must
// have a non-empty `doc` struct tag reachable via reflection.
func TestTier1FieldsHaveDocTags(t *testing.T) {
	docs := collectDocTags(t, DefaultConfig())
	for _, key := range tier1Fields {
		d, ok := docs[key]
		if !ok {
			t.Errorf("tier 1 field %q not reachable via reflection", key)
			continue
		}
		if d == "" {
			t.Errorf("tier 1 field %q has empty doc tag", key)
		}
	}
}

// TestTier1DocLengthBounds — CHECK-T16: every Tier 1 doc string
// must fall within [20,200] characters. Enforces discipline so
// one-word tooltips can't slip through.
func TestTier1DocLengthBounds(t *testing.T) {
	docs := collectDocTags(t, DefaultConfig())
	for _, key := range tier1Fields {
		d := docs[key]
		if d == "" {
			continue // reported by TestTier1FieldsHaveDocTags
		}
		n := len(d)
		if n < 20 || n > 200 {
			t.Errorf("tier 1 field %q doc length %d outside [20,200]",
				key, n)
		}
	}
}

// TestTier2FieldsDocumented — v0.8.5 progressive tightening per
// plan §7.6: every Tier 2 field must now carry a non-empty doc tag.
// Flipped from t.Log to t.Error once all 20 fields landed docs.
func TestTier2FieldsDocumented(t *testing.T) {
	docs := collectDocTags(t, DefaultConfig())
	var missing []string
	for _, key := range tier2Fields {
		if docs[key] == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		t.Errorf("tier 2 fields missing doc tags (%d): %v — "+
			"add doc:\"...\" struct tags in internal/config/config_types*.go "+
			"and run `go run ./cmd/gen_config_meta/`",
			len(missing), missing)
	}
}

// TestTier2DocLengthBounds — once a Tier 2 field has a doc tag it
// must respect the same [20,200] length envelope as Tier 1. Keeps
// documented fields coherent as new keys join the tier.
func TestTier2DocLengthBounds(t *testing.T) {
	docs := collectDocTags(t, DefaultConfig())
	for _, key := range tier2Fields {
		d := docs[key]
		if d == "" {
			continue // reported by TestTier2FieldsDocumented
		}
		n := len(d)
		if n < 20 || n > 200 {
			t.Errorf("tier 2 field %q doc length %d outside [20,200]",
				key, n)
		}
	}
}

// TestTier2ConfigMetaJSONMatches — drift guard for Tier 2 parallel
// to TestConfigMetaJSONExistsAndMatches. Forces a regenerate when
// Tier 2 doc tags change.
func TestTier2ConfigMetaJSONMatches(t *testing.T) {
	path := findConfigMetaPath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("config_meta.json not found at %s: %v", path, err)
	}
	var meta map[string]map[string]interface{}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal config_meta.json: %v", err)
	}
	docs := collectDocTags(t, DefaultConfig())
	for _, key := range tier2Fields {
		entry, ok := meta[key]
		if !ok {
			t.Errorf("tier 2 field %q missing from config_meta.json — "+
				"regenerate with `go run ./cmd/gen_config_meta/`", key)
			continue
		}
		jsonDoc, _ := entry["doc"].(string)
		if jsonDoc != docs[key] {
			t.Errorf("config_meta.json drift for %q:\n"+
				"  json = %q\n  go tag = %q\n"+
				"  regenerate with `go run ./cmd/gen_config_meta/`",
				key, jsonDoc, docs[key])
		}
	}
}

// TestConfigMetaJSONExistsAndMatches — CHECK-T04: the committed
// config_meta.json must contain entries for every Tier 1 field,
// with doc strings matching those reachable from live config.go.
// This catches "regenerate was forgotten" drift.
func TestConfigMetaJSONExistsAndMatches(t *testing.T) {
	path := findConfigMetaPath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("config_meta.json not found at %s: %v", path, err)
	}
	var meta map[string]map[string]interface{}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal config_meta.json: %v", err)
	}
	docs := collectDocTags(t, DefaultConfig())
	for _, key := range tier1Fields {
		entry, ok := meta[key]
		if !ok {
			t.Errorf("tier 1 field %q missing from config_meta.json — "+
				"regenerate with `go run ./cmd/gen_config_meta/`", key)
			continue
		}
		jsonDoc, _ := entry["doc"].(string)
		if jsonDoc != docs[key] {
			t.Errorf("config_meta.json drift for %q:\n "+
				"  json = %q\n  go tag = %q\n"+
				"  regenerate with `go run ./cmd/gen_config_meta/`",
				key, jsonDoc, docs[key])
		}
	}
}

// TestSecretFieldsMarked — CHECK-T11: every field whose yaml key
// matches the sensitive-name suffix pattern must carry secret:"true".
// We replicate the generator's detection locally rather than import
// from cmd/ (which would create a cycle).
func TestSecretFieldsMarked(t *testing.T) {
	secretSuffixes := []string{
		"password", "api_key", "apikey",
		"encryption_key", "client_secret",
		"tls_cert", "tls_key", "secret", "token",
	}
	isSensitive := func(k string) bool {
		lk := strings.ToLower(k)
		for _, s := range secretSuffixes {
			if lk == s || strings.HasSuffix(lk, "_"+s) {
				return true
			}
		}
		return false
	}
	walkSecrets(t, reflect.ValueOf(DefaultConfig()).Elem(), "",
		func(key string, sf reflect.StructField) {
			if !isSensitive(strings.SplitN(sf.Tag.Get("yaml"), ",", 2)[0]) {
				return
			}
			if sf.Tag.Get("secret") != "true" {
				t.Errorf("sensitive field %q missing secret:\"true\" tag",
					key)
			}
		})
}

// collectDocTags walks cfg via reflection and returns a map of
// dot-path key → doc tag value. Mirrors the generator's walking
// rules so the drift test sees the same key set.
func collectDocTags(t *testing.T, cfg *Config) map[string]string {
	t.Helper()
	out := make(map[string]string)
	walkDocs(reflect.ValueOf(cfg).Elem(), "", out)
	return out
}

func walkDocs(v reflect.Value, prefix string, out map[string]string) {
	tt := v.Type()
	for i := 0; i < tt.NumField(); i++ {
		sf := tt.Field(i)
		if !sf.IsExported() {
			continue
		}
		yamlTag := sf.Tag.Get("yaml")
		if yamlTag == "" || yamlTag == "-" {
			continue
		}
		name := strings.SplitN(yamlTag, ",", 2)[0]
		if name == "" {
			continue
		}
		fullKey := name
		if prefix != "" {
			fullKey = prefix + "." + name
		}
		fv := v.Field(i)
		ft := fv.Type()
		if ft.Kind() == reflect.Pointer {
			if fv.IsNil() {
				continue
			}
			fv = fv.Elem()
			ft = fv.Type()
		}
		switch ft.Kind() {
		case reflect.Struct:
			walkDocs(fv, fullKey, out)
			continue
		case reflect.Slice:
			elem := ft.Elem()
			if elem.Kind() == reflect.Struct {
				walkDocs(reflect.New(elem).Elem(), fullKey+"[]", out)
				continue
			}
		}
		out[fullKey] = sf.Tag.Get("doc")
	}
}

// walkSecrets walks cfg via reflection and invokes visit for every
// scalar or slice leaf with a yaml tag. Drives the secret-tag drift
// test.
func walkSecrets(
	t *testing.T, v reflect.Value, prefix string,
	visit func(key string, sf reflect.StructField),
) {
	t.Helper()
	tt := v.Type()
	for i := 0; i < tt.NumField(); i++ {
		sf := tt.Field(i)
		if !sf.IsExported() {
			continue
		}
		yamlTag := sf.Tag.Get("yaml")
		if yamlTag == "" || yamlTag == "-" {
			continue
		}
		name := strings.SplitN(yamlTag, ",", 2)[0]
		if name == "" {
			continue
		}
		fullKey := name
		if prefix != "" {
			fullKey = prefix + "." + name
		}
		fv := v.Field(i)
		ft := fv.Type()
		if ft.Kind() == reflect.Pointer {
			if fv.IsNil() {
				continue
			}
			fv = fv.Elem()
			ft = fv.Type()
		}
		switch ft.Kind() {
		case reflect.Struct:
			walkSecrets(t, fv, fullKey, visit)
			continue
		case reflect.Slice:
			elem := ft.Elem()
			if elem.Kind() == reflect.Struct {
				walkSecrets(t, reflect.New(elem).Elem(),
					fullKey+"[]", visit)
				continue
			}
		}
		visit(fullKey, sf)
	}
}

// findConfigMetaPath resolves sidecar/web/src/generated/config_meta.json
// relative to the test file's location so `go test` works regardless
// of the caller's cwd.
func findConfigMetaPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// file = .../sidecar/internal/config/meta_drift_test.go
	// Target = .../sidecar/web/src/generated/config_meta.json
	configDir := filepath.Dir(file)
	internalDir := filepath.Dir(configDir)
	sidecarDir := filepath.Dir(internalDir)
	return filepath.Join(
		sidecarDir, "web", "src", "generated", "config_meta.json")
}
