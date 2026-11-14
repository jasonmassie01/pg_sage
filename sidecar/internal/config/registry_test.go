package config

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

// config-02b: one tag-declared registry for YAML, the API and the
// dashboard. These tests use the package internals (the field index and
// the builder) that the cross-package parity tests cannot reach.

func TestRegistryHasEveryOverridableKeyWithAKnownType(t *testing.T) {
	overridable := 0
	for _, s := range Registry() {
		if !s.Overridable {
			continue
		}
		overridable++
		if _, ok := apiTypes[s.Type]; !ok {
			t.Errorf("%s has unknown api type %q", s.Key, s.Type)
		}
		if len(s.index) == 0 {
			t.Errorf("%s has no field index", s.Key)
		}
	}
	if overridable != 121 {
		t.Fatalf("overridable keys = %d, want the 121 the API stored before config-02b",
			overridable)
	}
}

func TestRegistryLookupMatchesTheTags(t *testing.T) {
	for key, want := range map[string]KeySpec{
		"collector.interval_seconds": {Type: "int_min5", HasRange: true, Integer: true,
			Min: 5, Max: MaxIntSetting},
		"analyzer.self_cost_budget_ms": {Type: "int_nonneg", HasRange: true,
			Integer: true, Min: 0, Max: 3600000},
		"safety.index_build_timeout_seconds": {Type: "int_class_timeout",
			HasRange: true, Integer: true, Min: 60, Max: 86400, ZeroDefault: true},
		"analyzer.wraparound_freeze_share": {Type: "float_share", HasRange: true,
			Min: 0.01, Max: 1},
		"mcp.transport": {Type: "mcp_transport", Enum: []string{"stdio", "http"}},
		"llm.auth":      {Type: "llm_auth", Enum: []string{"", "api_key", "databricks"}},
	} {
		got, ok := LookupKey(key)
		if !ok {
			t.Errorf("LookupKey(%s) missing", key)
			continue
		}
		if got.Type != want.Type || got.HasRange != want.HasRange ||
			got.Integer != want.Integer || got.Min != want.Min || got.Max != want.Max ||
			got.ZeroDefault != want.ZeroDefault || !slices.Equal(got.Enum, want.Enum) ||
			!got.Overridable {
			t.Errorf("LookupKey(%s) = %+v, want %+v", key, got, want)
		}
	}
}

// The trust.level tag must list exactly the levels the rest of the
// codebase ranks (TrustLevels); a fourth level added there fails here.
func TestTrustLevelEnumMatchesTrustLevels(t *testing.T) {
	spec, _ := LookupKey("trust.level")
	if !slices.Equal(spec.Enum, TrustLevels()) {
		t.Fatalf("trust.level enum %v, want TrustLevels() %v", spec.Enum, TrustLevels())
	}
}

// Nil/empty: a field with no contract tags has no spec and is accepted
// exactly as before.
func TestRegistryUntaggedFieldHasNoSpec(t *testing.T) {
	for _, key := range []string{"postgres.host", "llm.model", "mode"} {
		if s, ok := LookupKey(key); ok && (s.HasRange || len(s.Enum) > 0) {
			t.Errorf("%s has a contract %+v without tags", key, s)
		}
	}
	type untagged struct {
		N int    `yaml:"n"`
		S string `yaml:"s"`
	}
	if specs := mustBuildRegistry(reflect.TypeOf(untagged{})); len(specs) != 0 {
		t.Fatalf("untagged struct produced %+v", specs)
	}
	if _, ok := LookupKey(""); ok {
		t.Fatal(`LookupKey("") found a spec`)
	}
}

// Invalid input: every malformed range or enum tag panics in the builder,
// naming the key.
func TestRegistryMalformedRangeAndEnumTagsPanic(t *testing.T) {
	assertBuildPanics(t, map[string]reflect.Type{
		"range one number": reflect.TypeOf(struct {
			F int `yaml:"f" range:"5"`
		}{}),
		"range not numbers": reflect.TypeOf(struct {
			F int `yaml:"f" range:"a,b"`
		}{}),
		"range reversed": reflect.TypeOf(struct {
			F int `yaml:"f" range:"9,1"`
		}{}),
		"range fraction on int": reflect.TypeOf(struct {
			F int `yaml:"f" range:"0.5,1"`
		}{}),
		"range on string": reflect.TypeOf(struct {
			F string `yaml:"f" range:"0,1"`
		}{}),
		"empty enum": reflect.TypeOf(struct {
			F string `yaml:"f" enum:""`
		}{}),
		"duplicate enum": reflect.TypeOf(struct {
			F string `yaml:"f" enum:"a|a"`
		}{}),
		"enum on int": reflect.TypeOf(struct {
			F int `yaml:"f" enum:"a|b"`
		}{}),
		// validateRegistry reads a leaf by kind; a pointer would go unchecked.
		"range on pointer int": reflect.TypeOf(struct {
			F *int `yaml:"f" range:"1,9"`
		}{}),
		"enum on pointer string": reflect.TypeOf(struct {
			F *string `yaml:"f" enum:"a|b"`
		}{}),
		"ranged api on pointer int": reflect.TypeOf(struct {
			F *int `yaml:"f" api:"int_pos"`
		}{}),
	})
}

// Invalid input: every malformed api or zero tag panics in the builder.
func TestRegistryMalformedAPIAndZeroTagsPanic(t *testing.T) {
	assertBuildPanics(t, map[string]reflect.Type{
		"unknown api type": reflect.TypeOf(struct {
			F int `yaml:"f" api:"int_maybe"`
		}{}),
		"numeric api on string": reflect.TypeOf(struct {
			F string `yaml:"f" api:"int_pos"`
		}{}),
		"int api on float": reflect.TypeOf(struct {
			F float64 `yaml:"f" api:"int_pos"`
		}{}),
		"enum type without enum": reflect.TypeOf(struct {
			F string `yaml:"f" api:"mcp_transport"`
		}{}),
		"zero not default": reflect.TypeOf(struct {
			F int `yaml:"f" api:"int_pos" zero:"maybe"`
		}{}),
		"zero without range": reflect.TypeOf(struct {
			F string `yaml:"f" zero:"default"`
		}{}),
		"zero on nonneg range": reflect.TypeOf(struct {
			F int `yaml:"f" api:"int_nonneg" zero:"default"`
		}{}),
	})
}

func assertBuildPanics(t *testing.T, cases map[string]reflect.Type) {
	t.Helper()
	for name, rt := range cases {
		t.Run(name, func(t *testing.T) {
			msg := capturePanic(func() { mustBuildRegistry(rt) })
			if !strings.Contains(msg, "config registry: f") {
				t.Fatalf("panic %q, want one naming key f", msg)
			}
		})
	}
}

func TestRegistryNestedAndPointerSectionsGetDottedKeys(t *testing.T) {
	type inner struct {
		N int `yaml:"n" range:"1,9"`
	}
	type outer struct {
		In  inner  `yaml:"in"`
		Ptr *inner `yaml:"ptr"`
	}
	specs := mustBuildRegistry(reflect.TypeOf(outer{}))
	if len(specs) != 2 || specs[0].Key != "in.n" || specs[1].Key != "ptr.n" {
		t.Fatalf("specs = %+v, want in.n and ptr.n", specs)
	}
	if specs[0].Overridable || !specs[0].HasRange || specs[0].Min != 1 || specs[0].Max != 9 {
		t.Fatalf("in.n = %+v, want a YAML-only 1-9 range", specs[0])
	}
}

// Default masking (CLAUDE.md): with no YAML at all, every default is
// inside its own range and enum.
func TestDefaultConfigPassesEveryRegistryContract(t *testing.T) {
	cfg := DefaultConfig()
	root := reflect.ValueOf(cfg).Elem()
	checked := 0
	for _, s := range Registry() {
		v, err := root.FieldByIndexErr(s.index)
		if err != nil {
			continue
		}
		if err := s.checkValue(v); err != nil {
			t.Errorf("default %s fails its own contract: %v", s.Key, err)
		}
		checked++
	}
	if checked < 100 {
		t.Fatalf("checked %d defaults; the registry lost its subject", checked)
	}
	if err := cfg.validateRegistry(); err != nil {
		t.Fatalf("validateRegistry(DefaultConfig()) = %v", err)
	}
}

func TestKeySpecCheckNumberBoundaries(t *testing.T) {
	timeout, _ := LookupKey("safety.index_build_timeout_seconds")
	share, _ := LookupKey("analyzer.wraparound_freeze_share")
	for _, tc := range []struct {
		spec KeySpec
		n    float64
		ok   bool
	}{
		{timeout, 0, true}, {timeout, 59, false}, {timeout, 60, true},
		{timeout, 86400, true}, {timeout, 86401, false}, {timeout, -1, false},
		{share, 0.01, true}, {share, 1, true}, {share, 0.009, false}, {share, 0, false},
		{KeySpec{Key: "free"}, -1e12, true},
	} {
		err := tc.spec.CheckNumber(tc.n)
		if (err == nil) != tc.ok {
			t.Errorf("%s CheckNumber(%v) = %v, want ok=%v", tc.spec.Key, tc.n, err, tc.ok)
		}
		if err != nil && !strings.HasPrefix(err.Error(), tc.spec.Key+" must be ") {
			t.Errorf("error %q does not lead with the key", err)
		}
	}
	err := timeout.CheckNumber(59)
	if err == nil || err.Error() != "safety.index_build_timeout_seconds must be 60-86400, got 59" {
		t.Fatalf("message = %v", err)
	}
	// A stored override of 0 is refused: DELETE restores the default.
	err = timeout.CheckOverride(0)
	if err == nil || !strings.Contains(err.Error(), "remove the override") {
		t.Fatalf("CheckOverride(0) = %v, want a refusal pointing at removal", err)
	}
	if err := timeout.CheckOverride(60); err != nil {
		t.Fatalf("CheckOverride(60) = %v", err)
	}
	if err := share.CheckNumber(2); err == nil ||
		!strings.Contains(err.Error(), "0.01-1, got 2") {
		t.Fatalf("float message = %v", err)
	}
}

func TestKeySpecCheckEnum(t *testing.T) {
	auth, _ := LookupKey("llm.auth")
	for v, ok := range map[string]bool{"": true, "api_key": true, "databricks": true,
		"API_KEY": false, "oauth": false} {
		if err := auth.CheckEnum(v); (err == nil) != ok {
			t.Errorf("llm.auth CheckEnum(%q) = %v, want ok=%v", v, err, ok)
		}
	}
	err := auth.CheckEnum("oauth")
	if err == nil || err.Error() !=
		`llm.auth must be one of "", api_key, databricks, got "oauth"` {
		t.Fatalf("message = %v", err)
	}
	if err := (KeySpec{Key: "free"}).CheckEnum("anything"); err != nil {
		t.Fatalf("a key with no enum refused a value: %v", err)
	}
}

// Concurrent access: eight goroutines see one registry built once.
func TestRegistryBuildsOnceUnderConcurrentCallers(t *testing.T) {
	const callers = 8
	var wg sync.WaitGroup
	got := make([][]KeySpec, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = Registry()
		}(i)
	}
	wg.Wait()
	for i := 1; i < callers; i++ {
		if len(got[i]) == 0 || &got[i][0] != &got[0][0] {
			t.Fatalf("caller %d got a different registry slice", i)
		}
	}
}

// State transition: a hot-reload candidate with a value the registry now
// refuses (llm.timeout_seconds 0, which YAML accepted before config-02b)
// keeps the previous generation, as every invalid candidate does.
func TestResyncRejectsCandidateOutsideARegistryRange(t *testing.T) {
	owner := &countingOwner{name: LLMOwner}
	controller := NewConfigController(DefaultConfig(), nil, owner)
	_, adopted, err := controller.Resync(context.Background(), 4, timeoutCandidate(0))
	if !errors.Is(err, ErrInvalidCandidate) || adopted {
		t.Fatalf("Resync = adopted %v, err %v; want ErrInvalidCandidate", adopted, err)
	}
	if !strings.Contains(err.Error(), "llm.timeout_seconds") {
		t.Fatalf("error %q does not name llm.timeout_seconds", err)
	}
	assertGenerations(t, controller, 1, 1)
	if _, adopted, err := controller.Resync(context.Background(), 4,
		timeoutCandidate(45)); err != nil || !adopted {
		t.Fatalf("a valid candidate after the rejected one: adopted %v, err %v", adopted, err)
	}
}

func capturePanic(f func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg, _ = r.(string)
		}
	}()
	f()
	return ""
}
