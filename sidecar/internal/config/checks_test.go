package config

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

// market-11: rule packs (analyzer.disabled_rules) and owner checks. The
// SQL refusals that need the parser are in checks_cgo_test.go; a build
// without cgo refuses every check (checks_nocgo_test.go).

func validCheck() CustomCheck {
	return CustomCheck{
		ID: "table_without_pk", Title: "Table without a primary key",
		Severity: "warning", SQL: "SELECT relname FROM pg_class",
		IntervalMinutes: 15, MaxRows: 50, TimeoutMs: 1000,
		ObjectColumn: "relname", Message: "Add a primary key.",
	}
}

func TestChecks_ShapeRefusals(t *testing.T) {
	cases := []struct {
		name string
		edit func(*CustomCheck)
		want string
	}{
		{"empty sql", func(c *CustomCheck) { c.SQL = "  " }, "sql is empty"},
		{"timeout 6000", func(c *CustomCheck) { c.TimeoutMs = 6000 }, "timeout_ms"},
		{"interval 4", func(c *CustomCheck) { c.IntervalMinutes = 4 }, "interval_minutes"},
		{"max rows 101", func(c *CustomCheck) { c.MaxRows = 101 }, "max_rows"},
		{"negative max rows", func(c *CustomCheck) { c.MaxRows = -1 }, "max_rows"},
		{"empty id", func(c *CustomCheck) { c.ID = "" }, "id"},
		{"bad id", func(c *CustomCheck) { c.ID = "Bad-ID" }, "id"},
		{"no title", func(c *CustomCheck) { c.Title = "" }, "title"},
		{"bad severity", func(c *CustomCheck) { c.Severity = "high" }, "severity"},
		{"no object column", func(c *CustomCheck) { c.ObjectColumn = "" }, "object_column"},
		{"bad glob", func(c *CustomCheck) { c.Databases = []string{"[a"} }, "databases"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validCheck()
			tc.edit(&c)
			err := validateCheckShape("checks[0]", c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateCheckShape = %v, want an error naming %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "checks[0]") {
				t.Fatalf("error %q does not name the entry", err)
			}
		})
	}
}

func TestChecks_ShapeBoundariesAccepted(t *testing.T) {
	c := validCheck()
	c.IntervalMinutes, c.MaxRows, c.TimeoutMs = 5, 100, 5000
	if err := validateCheckShape("checks[0]", c); err != nil {
		t.Fatalf("interval 5, max_rows 100, timeout 5000: %v", err)
	}
	c.MaxRows, c.TimeoutMs = 1, 1
	if err := validateCheckShape("checks[0]", c); err != nil {
		t.Fatalf("max_rows 1, timeout 1: %v", err)
	}
}

func TestChecks_DefaultsFillZeroValues(t *testing.T) {
	c := validCheck()
	c.IntervalMinutes, c.MaxRows, c.TimeoutMs, c.Databases = 0, 0, 0, nil
	got := c.withDefaults()
	if got.IntervalMinutes != DefaultCheckIntervalMinutes ||
		got.MaxRows != MaxCheckRows || got.TimeoutMs != DefaultCheckTimeoutMs {
		t.Fatalf("defaults = %d/%d/%d, want %d/%d/%d", got.IntervalMinutes,
			got.MaxRows, got.TimeoutMs, DefaultCheckIntervalMinutes, MaxCheckRows,
			DefaultCheckTimeoutMs)
	}
	if len(got.Databases) != 1 || got.Databases[0] != "*" {
		t.Fatalf("databases default = %v, want [*]", got.Databases)
	}
	if err := validateCheckShape("checks[0]", got); err != nil {
		t.Fatalf("defaulted check refused: %v", err)
	}
}

func TestChecks_DuplicateIDRefused(t *testing.T) {
	_, err := PrepareChecks([]CustomCheck{validCheck(), validCheck()})
	if err == nil || !strings.Contains(err.Error(), "duplicate id") {
		t.Fatalf("PrepareChecks = %v, want a duplicate id error", err)
	}
}

func TestChecks_NilAndEmptyPrepareNothing(t *testing.T) {
	for _, in := range [][]CustomCheck{nil, {}} {
		got, err := PrepareChecks(in)
		if err != nil || len(got) != 0 {
			t.Fatalf("PrepareChecks(%v) = %v, %v; want none, nil", in, got, err)
		}
	}
}

func TestChecks_CategoryAndDatabaseGlob(t *testing.T) {
	c := validCheck().withDefaults()
	if c.Category() != "custom:table_without_pk" {
		t.Fatalf("Category = %q", c.Category())
	}
	if !c.AppliesTo("orders") || !c.AppliesTo("") {
		t.Fatal("the default * glob must match every database")
	}
	c.Databases = []string{"report_*", "billing"}
	for db, want := range map[string]bool{
		"report_eu": true, "billing": true, "billing2": false, "orders": false,
	} {
		if got := c.AppliesTo(db); got != want {
			t.Errorf("AppliesTo(%q) = %v, want %v", db, got, want)
		}
	}
}

func TestDisabledRules_Validation(t *testing.T) {
	ok := [][]string{nil, {}, {"unused_indexes"}, {"unused_index", "slow_queries"},
		{"inactive_slots"}, {"query_regression"}}
	for _, names := range ok {
		if err := ValidateDisabledRules("analyzer.disabled_rules", names); err != nil {
			t.Errorf("ValidateDisabledRules(%v) = %v, want nil", names, err)
		}
	}
	refused := map[string]string{
		"wraparound_freeze":   "cannot be disabled",
		"xid_wraparound":      "cannot be disabled",
		"sequence_exhaustion": "cannot be disabled",
		"sage_self_cost":      "cannot be disabled",
		"sage_footprint":      "cannot be disabled",
		"no_such_rule":        "unknown rule or category",
		"schema_lint:lint_x":  "unknown rule or category",
		"":                    "empty",
	}
	for name, want := range refused {
		err := ValidateDisabledRules("analyzer.disabled_rules", []string{name})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateDisabledRules(%q) = %v, want %q", name, err, want)
		}
		if err != nil && !strings.Contains(err.Error(), "analyzer.disabled_rules") {
			t.Errorf("error %q does not name the field", err)
		}
	}
}

func TestParseRuleList(t *testing.T) {
	got := ParseRuleList(" unused_indexes, ,slow_queries ,")
	if len(got) != 2 || got[0] != "unused_indexes" || got[1] != "slow_queries" {
		t.Fatalf("ParseRuleList = %q", got)
	}
	if got := ParseRuleList(""); len(got) != 0 || got == nil {
		t.Fatalf("ParseRuleList(\"\") = %#v, want an empty non-nil list", got)
	}
}

func TestDisabledRules_LoadRefusesProtectedGloballyAndPerDatabase(t *testing.T) {
	_, err := loadYAMLResult(t, standaloneIOYAML+`
analyzer:
  disabled_rules: [unused_indexes, wraparound_freeze]
`)
	if err == nil || !strings.Contains(err.Error(), "wraparound_freeze") {
		t.Fatalf("global load = %v, want wraparound_freeze refused", err)
	}
	cfg, err := loadYAMLResult(t, standaloneIOYAML+`
analyzer:
  disabled_rules: [unused_indexes]
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Analyzer.DisabledRules) != 1 || cfg.Analyzer.DisabledRules[0] != "unused_indexes" {
		t.Fatalf("disabled_rules = %v", cfg.Analyzer.DisabledRules)
	}
	db := &Config{Databases: []DatabaseConfig{{Name: "a",
		DisabledRules: []string{"sequence_exhaustion"}}}}
	if err := db.validateRulePacks(); err == nil ||
		!strings.Contains(err.Error(), "databases[0]") {
		t.Fatalf("per-database validate = %v, want databases[0] refused", err)
	}
}

func TestDisabledRules_DefaultIsEmpty(t *testing.T) {
	cfg, err := loadYAMLResult(t, standaloneIOYAML)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Analyzer.DisabledRules) != 0 || len(cfg.Checks) != 0 {
		t.Fatalf("defaults = %v / %v, want no disabled rules and no checks",
			cfg.Analyzer.DisabledRules, cfg.Checks)
	}
}

func TestChecks_LoadRefusesShapeBeforeParsing(t *testing.T) {
	_, err := loadYAMLResult(t, standaloneIOYAML+`
checks:
  - id: slow
    title: Slow
    severity: warning
    sql: SELECT 1 AS x
    object_column: x
    timeout_ms: 6000
`)
	if err == nil || !strings.Contains(err.Error(), "timeout_ms") {
		t.Fatalf("load = %v, want timeout_ms refused", err)
	}
}

// Concurrency: validation reads only its arguments and the init-built
// category registry; parallel callers must agree.
func TestDisabledRules_ConcurrentValidation(t *testing.T) {
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			names := []string{"unused_indexes"}
			if i%2 == 1 {
				names = append(names, "wraparound_freeze")
			}
			err := ValidateDisabledRules("f", names)
			if (err == nil) == (i%2 == 1) {
				errs <- errors.New("inconsistent verdict")
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
