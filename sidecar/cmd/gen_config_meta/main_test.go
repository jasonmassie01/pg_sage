package main

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// TestGenerate_SmokeDefaultConfig — baseline: generator runs against
// config.DefaultConfig() without error and produces at least the
// known tier 1 fields (CHECK-T09 scope).
func TestGenerate_SmokeDefaultConfig(t *testing.T) {
	meta, err := Generate(config.DefaultConfig(), false)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	tier1 := []string{
		"trust.level",
		"trust.maintenance_window",
		"trust.rollback_threshold_pct",
		"trust.rollback_window_minutes",
		"trust.rollback_cooldown_days",
		"safety.query_timeout_ms",
		"safety.lock_timeout_ms",
		"safety.ddl_timeout_seconds",
		"safety.cpu_ceiling_pct",
		"safety.backoff_consecutive_skips",
		"tuner.verify_after_apply",
	}
	for _, k := range tier1 {
		entry, ok := meta[k]
		if !ok {
			t.Errorf("tier 1 field %q missing from generated meta", k)
			continue
		}
		if entry.Doc == "" {
			t.Errorf("tier 1 field %q has empty doc", k)
		}
	}
}

// TestDeterministicOutput — CHECK-T03: two consecutive invocations
// of marshalSorted yield byte-identical bytes.
func TestDeterministicOutput(t *testing.T) {
	meta, err := Generate(config.DefaultConfig(), false)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	a, err := marshalSorted(meta)
	if err != nil {
		t.Fatalf("marshalSorted: %v", err)
	}
	b, err := marshalSorted(meta)
	if err != nil {
		t.Fatalf("marshalSorted 2: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("non-deterministic output:\nfirst=%s\nsecond=%s",
			string(a), string(b))
	}
}

// TestSecretDefaultNil — CHECK-T11: fields tagged secret:"true"
// never leak a default value into config_meta.json.
func TestSecretDefaultNil(t *testing.T) {
	meta, err := Generate(config.DefaultConfig(), false)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	api, ok := meta["llm.api_key"]
	if !ok {
		t.Fatal("llm.api_key missing from meta")
	}
	if !api.Secret {
		t.Errorf("llm.api_key Secret = false, want true")
	}
	if api.Default != nil {
		t.Errorf("llm.api_key Default = %v, want nil (secret)",
			api.Default)
	}
}

// TestSecretRegexEnforcement — CHECK-T11: a synthetic struct whose
// field name matches the sensitive-name regex but lacks secret:"true"
// must cause Generate to fail.
func TestSecretRegexEnforcement(t *testing.T) {
	type bad struct {
		APIKey string `yaml:"api_key" doc:"Unredacted API key should be refused by the generator."`
	}
	_, err := walkAny(&bad{}, false)
	if err == nil {
		t.Fatal("expected generator to refuse api_key without secret tag")
	}
	if !strings.Contains(err.Error(), "CHECK-T11") {
		t.Errorf("error should cite CHECK-T11, got: %v", err)
	}
}

// TestYamlTagOptionsParsing — CHECK-T12: yaml:"name,omitempty" →
// emitted key is "name", options after the comma are discarded.
func TestYamlTagOptionsParsing(t *testing.T) {
	type s struct {
		F int `yaml:"my_field,omitempty" doc:"A valid 20+ character documentation line for this field."`
	}
	out, err := walkAny(&s{F: 7}, false)
	if err != nil {
		t.Fatalf("walkAny: %v", err)
	}
	if _, ok := out["my_field"]; !ok {
		t.Errorf("expected key my_field, got %v", keysOf(out))
	}
	if _, ok := out["my_field,omitempty"]; ok {
		t.Error("key my_field,omitempty must not appear")
	}
}

// TestSliceOfStructKeyFormat — CHECK-T13: slice-of-struct fields
// emit element metadata under the parent[].child key format.
func TestSliceOfStructKeyFormat(t *testing.T) {
	type row struct {
		Name string `yaml:"name" doc:"Logical row name long enough to satisfy the 20-character minimum."`
	}
	type s struct {
		Rows []row `yaml:"rows" doc:"A slice of rows — doc tag on the slice itself is ignored by the generator."`
	}
	out, err := walkAny(&s{}, false)
	if err != nil {
		t.Fatalf("walkAny: %v", err)
	}
	key := "rows[].name"
	if _, ok := out[key]; !ok {
		t.Errorf("expected %q, got %v", key, keysOf(out))
	}
}

// TestMapOpaque — CHECK-T14: maps are opaque — one entry per map
// field, type string "map[K]V", no recursion into value types.
func TestMapOpaque(t *testing.T) {
	type s struct {
		Headers map[string]string `yaml:"headers" doc:"HTTP headers attached to each outgoing webhook call."`
	}
	out, err := walkAny(&s{Headers: map[string]string{"a": "b"}}, false)
	if err != nil {
		t.Fatalf("walkAny: %v", err)
	}
	h, ok := out["headers"]
	if !ok {
		t.Fatalf("headers missing, got %v", keysOf(out))
	}
	if h.Type != "map[string]string" {
		t.Errorf("Type = %q, want map[string]string", h.Type)
	}
}

// TestYamlDashSkipped — CHECK-T15: fields with yaml:"-" are not
// emitted. Uses the real Config to catch regressions in runtime fields
// like ConfigPath / PGVersionNum that must never appear.
func TestYamlDashSkipped(t *testing.T) {
	meta, err := Generate(config.DefaultConfig(), false)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	forbidden := []string{
		"", // empty yaml-name must not appear
		"config_path",
		"pg_version_num",
		"has_wal_columns",
		"has_plan_time_columns",
		"cloud_environment",
	}
	for _, k := range forbidden {
		if _, ok := meta[k]; ok {
			t.Errorf("forbidden key %q appears in meta", k)
		}
	}
}

// TestDocLengthBounds — CHECK-T16: generator refuses doc tags shorter
// than 20 or longer than 200 characters, with the field name in the
// error message.
func TestDocLengthBounds(t *testing.T) {
	type short struct {
		F int `yaml:"f" doc:"too short"`
	}
	_, err := walkAny(&short{}, false)
	if err == nil {
		t.Fatal("expected error for doc shorter than 20 chars")
	}
	if !strings.Contains(err.Error(), "CHECK-T16") {
		t.Errorf("error should cite CHECK-T16, got: %v", err)
	}

	type long struct {
		F int `yaml:"f" doc:"Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. Ut enim ad minim veniam, quis nostrud exercitation ullamco laboris nisi ut aliquip ex ea commodo consequat duis aute."`
	}
	_, err = walkAny(&long{}, false)
	if err == nil {
		t.Fatal("expected error for doc longer than 200 chars")
	}
	if !strings.Contains(err.Error(), "CHECK-T16") {
		t.Errorf("error should cite CHECK-T16, got: %v", err)
	}
}

// TestMalformedYamlTag — invalid input category per CLAUDE.md §5.
// Empty yaml:"" tag is silently skipped (not an error — matches the
// generator's contract that only tagged fields are considered).
func TestMalformedYamlTag(t *testing.T) {
	type s struct {
		Good  int `yaml:"good" doc:"Present in output because its yaml tag is non-empty and valid."`
		Blank int `yaml:""`
	}
	out, err := walkAny(&s{}, false)
	if err != nil {
		t.Fatalf("walkAny: %v", err)
	}
	if _, ok := out["good"]; !ok {
		t.Errorf("good missing, got %v", keysOf(out))
	}
	if _, ok := out[""]; ok {
		t.Error("empty-name field must not be emitted")
	}
}

// TestStrictMode — CHECK-T01: with strict=true, any yaml-tagged
// field lacking a doc tag causes Generate to fail.
func TestStrictMode(t *testing.T) {
	type s struct {
		F int `yaml:"undocumented"`
	}
	_, err := walkAny(&s{}, true)
	if err == nil {
		t.Fatal("expected strict mode to fail on undocumented field")
	}
	if !strings.Contains(err.Error(), "CHECK-T01") {
		t.Errorf("error should cite CHECK-T01, got: %v", err)
	}
}

// TestScalarDefaults — boundary coverage: verify scalarDefault emits
// the correct concrete type for every primitive Kind the generator
// might encounter in future config fields. Ensures adding a uint or
// float field to Config doesn't silently emit null.
func TestScalarDefaults(t *testing.T) {
	type s struct {
		B   bool    `yaml:"b" doc:"Boolean field with sufficient doc length for validation."`
		I   int     `yaml:"i" doc:"Signed integer field with sufficient doc length for validation."`
		I64 int64   `yaml:"i64" doc:"Signed 64-bit field with sufficient doc length for validation."`
		U   uint    `yaml:"u" doc:"Unsigned integer field with sufficient doc length for validation."`
		U64 uint64  `yaml:"u64" doc:"Unsigned 64-bit field with sufficient doc length for validation."`
		F32 float32 `yaml:"f32" doc:"32-bit float field with sufficient doc length for validation."`
		F64 float64 `yaml:"f64" doc:"64-bit float field with sufficient doc length for validation."`
		Str string  `yaml:"str" doc:"String field with sufficient doc length for validation."`
	}
	sample := s{
		B: true, I: -3, I64: 1 << 40,
		U: 7, U64: 1 << 40,
		F32: 1.5, F64: 2.5,
		Str: "hello",
	}
	out, err := walkAny(&sample, false)
	if err != nil {
		t.Fatalf("walkAny: %v", err)
	}
	cases := []struct {
		key  string
		want interface{}
	}{
		{"b", true},
		{"i", int64(-3)},
		{"i64", int64(1 << 40)},
		{"u", uint64(7)},
		{"u64", uint64(1 << 40)},
		{"f64", 2.5},
		{"str", "hello"},
	}
	for _, tc := range cases {
		got := out[tc.key].Default
		if got != tc.want {
			t.Errorf("%s default = %v (%T), want %v (%T)",
				tc.key, got, got, tc.want, tc.want)
		}
	}
}

// TestRun_HappyPath — exercises the full run() path end-to-end by
// writing to a temporary file and verifying the bytes are valid JSON
// with the expected tier 1 keys. Covers the happy path of main().
func TestRun_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	out := tmp + "/nested/config_meta.json"
	if err := run([]string{"-out", out}); err != nil {
		t.Fatalf("run: %v", err)
	}
	data, err := readFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if len(data) < 100 {
		t.Errorf("output too small: %d bytes", len(data))
	}
	if !strings.Contains(string(data), `"trust.level"`) {
		t.Error("output missing tier 1 key trust.level")
	}
}

// TestRun_StrictFails — run() with -strict must fail against the
// real config because not every field has a doc tag yet.
func TestRun_StrictFails(t *testing.T) {
	tmp := t.TempDir()
	out := tmp + "/config_meta.json"
	err := run([]string{"-out", out, "-strict"})
	if err == nil {
		t.Fatal("expected strict mode to fail")
	}
	if !strings.Contains(err.Error(), "CHECK-T01") {
		t.Errorf("error should cite CHECK-T01, got: %v", err)
	}
}

// TestRun_BadFlag — flag parser error surfaces as a non-nil run()
// error rather than an os.Exit.
func TestRun_BadFlag(t *testing.T) {
	if err := run([]string{"-nope"}); err == nil {
		t.Fatal("expected error for unknown flag")
	}
}

func TestWave5RunLifecycleOnlyWritesTypedRegistry(t *testing.T) {
	out := t.TempDir() + "/config-lifecycles.md"
	if err := run([]string{
		"-lifecycle-only", "-lifecycle-out", out,
	}); err != nil {
		t.Fatalf("run lifecycle-only: %v", err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read lifecycle output: %v", err)
	}
	if got, want := string(body), config.ConfigLifecycleMarkdown(); got != want {
		t.Fatal("lifecycle-only output did not match the typed registry")
	}
}

func TestWave5RunLifecycleOnlyRequiresOutput(t *testing.T) {
	err := run([]string{"-lifecycle-only"})
	if err == nil || !strings.Contains(err.Error(), "requires -lifecycle-out") {
		t.Fatalf("run lifecycle-only error = %v, want output requirement", err)
	}
}

// readFile is a small helper to avoid importing os/ioutil redundantly.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// TestParentDir — utility coverage for the output-directory helper.
func TestParentDir(t *testing.T) {
	cases := map[string]string{
		"a/b/c":      "a/b",
		`a\b\c`:      `a\b`,
		"no_slashes": ".",
		"/abs/path":  "/abs",
	}
	for in, want := range cases {
		got := parentDir(in)
		if got != want {
			t.Errorf("parentDir(%q) = %q, want %q", in, got, want)
		}
	}
}

// walkAny is a test helper that wraps walkStruct for arbitrary struct
// pointers so tests don't need to construct a full config.Config.
func walkAny(ptr interface{}, strict bool) (map[string]fieldMeta, error) {
	v := reflect.ValueOf(ptr).Elem()
	out := make(map[string]fieldMeta)
	if err := walkStruct(v, "", out, strict); err != nil {
		return nil, err
	}
	return out, nil
}

func keysOf(m map[string]fieldMeta) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
