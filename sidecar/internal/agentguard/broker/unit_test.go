package broker

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// No concurrency tests in this file: every function here is pure. The
// shared state (pools, the connection budget) is exercised under
// concurrency in broker_db_test.go.

func TestForbiddenRune(t *testing.T) {
	bad := []struct {
		name string
		r    rune
	}{
		{"soft hyphen (Cf)", '\u00AD'},
		{"private use (Co)", '\uE000'},
		{"tag block", '\U000E0041'},
		{"tag block end", '\U000E007F'},
		{"bidi override", '\u202E'},
		{"bidi embedding start", '\u202A'},
		{"bidi isolate", '\u2066'},
		{"bidi isolate end", '\u2069'},
		{"zero-width space", '\u200B'},
		{"zero-width joiner", '\u200D'},
		{"byte order mark", '\uFEFF'},
	}
	for _, c := range bad {
		sql := "SELECT 'a" + string(c.r) + "b'"
		r, at, found := forbiddenRune(sql)
		if !found || r != c.r || at != len("SELECT 'a") {
			t.Errorf("%s: forbiddenRune = %U at %d found=%v, want %U at %d", c.name, r, at,
				found, c.r, len("SELECT 'a"))
		}
	}
	for _, ok := range []string{"SELECT 'caf\u00E9', '\u00DCn\u00EFc\u00F6d\u00E9', '\u65E5\u672C'",
		"SELECT 1\n\tFROM t",
		"", "SELECT 'emoji \U0001F600'"} {
		if r, _, found := forbiddenRune(ok); found {
			t.Errorf("%q: flagged %U, want accepted", ok, r)
		}
	}
	// Invalid UTF-8 cannot be screened, so it is refused too.
	if _, _, found := forbiddenRune("SELECT '\xff'"); !found {
		t.Error("invalid UTF-8 was accepted")
	}
}

func TestDefaultConfigMatchesSpec(t *testing.T) {
	c := DefaultConfig()
	if c.MaxRows != 200 || c.MaxRowsCeiling != 1000 || c.MaxBytes != 1<<20 {
		t.Errorf("row/byte defaults = %d/%d/%d, want 200/1000/1048576", c.MaxRows,
			c.MaxRowsCeiling, c.MaxBytes)
	}
	if c.StatementTimeout != 30*time.Second || c.LockTimeout != time.Second {
		t.Errorf("timeouts = %v/%v, want 30s/1s", c.StatementTimeout, c.LockTimeout)
	}
	if c.PoolMaxConns != 2 || c.MaxTotalConns != 20 || c.PoolIdle != time.Minute {
		t.Errorf("pool defaults = %d/%d/%v, want 2/20/1m", c.PoolMaxConns, c.MaxTotalConns,
			c.PoolIdle)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("default config invalid: %v", err)
	}
}

func TestConfigValidateRejectsBadBounds(t *testing.T) {
	mutations := map[string]func(*Config){
		"zero max rows":        func(c *Config) { c.MaxRows = 0 },
		"ceiling below rows":   func(c *Config) { c.MaxRowsCeiling = c.MaxRows - 1 },
		"zero bytes":           func(c *Config) { c.MaxBytes = 0 },
		"zero timeout":         func(c *Config) { c.StatementTimeout = 0 },
		"sub-ms timeout":       func(c *Config) { c.StatementTimeout = time.Microsecond },
		"zero lock timeout":    func(c *Config) { c.LockTimeout = 0 },
		"zero pool":            func(c *Config) { c.PoolMaxConns = 0 },
		"total below pool":     func(c *Config) { c.MaxTotalConns = 1 },
		"zero idle":            func(c *Config) { c.PoolIdle = 0 },
	}
	for name, mutate := range mutations {
		c := DefaultConfig()
		mutate(&c)
		if err := c.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
	c := DefaultConfig()
	c.MaxRowsCeiling = c.MaxRows // boundary: equal is allowed
	if err := c.Validate(); err != nil {
		t.Errorf("ceiling == max_rows rejected: %v", err)
	}
}

func TestValidateRequest(t *testing.T) {
	cfg := DefaultConfig()
	ok := Request{Database: "db", SQL: "SELECT 1"}
	rows, params, err := validateRequest(cfg, ok)
	if err != nil || rows != cfg.MaxRows || len(params) != 0 {
		t.Fatalf("default request = %d, %v, %v; want %d rows", rows, params, err, cfg.MaxRows)
	}
	ceiling := ok
	ceiling.MaxRows = cfg.MaxRowsCeiling
	if rows, _, err := validateRequest(cfg, ceiling); err != nil || rows != cfg.MaxRowsCeiling {
		t.Errorf("max_rows == ceiling = %d, %v; want accepted", rows, err)
	}
	bad := map[string]Request{
		"empty sql":        {Database: "db", SQL: "  "},
		"no database":      {SQL: "SELECT 1"},
		"over ceiling":     {Database: "db", SQL: "SELECT 1", MaxRows: cfg.MaxRowsCeiling + 1},
		"negative rows":    {Database: "db", SQL: "SELECT 1", MaxRows: -1},
		"huge sql":         {Database: "db", SQL: "SELECT " + strings.Repeat("1", MaxSQLBytes)},
		"object parameter": {Database: "db", SQL: "SELECT $1", Params: []any{map[string]any{}}},
		"array parameter":  {Database: "db", SQL: "SELECT $1", Params: []any{[]any{1}}},
		"too many params":  {Database: "db", SQL: "SELECT 1", Params: make([]any, MaxParams+1)},
	}
	for name, req := range bad {
		if _, _, err := validateRequest(cfg, req); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: error = %v, want ErrInvalid", name, err)
		}
	}
}

func TestParamText(t *testing.T) {
	_, params, err := validateRequest(DefaultConfig(), Request{Database: "d", SQL: "SELECT 1",
		Params: []any{"x", json.Number("12345678901234567890"), 2.5, true, false, nil, 7}})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	want := []string{"x", "12345678901234567890", "2.5", "true", "false", "<nil>", "7"}
	for i, p := range params {
		got := "<nil>"
		if p != nil {
			got = string(p)
		}
		if got != want[i] {
			t.Errorf("param %d = %q, want %q", i, got, want[i])
		}
	}
}

func TestEnvelopeHash(t *testing.T) {
	base := envelopeHash("agp_a", "db-1", "SELECT 1", [][]byte{[]byte("x")})
	if len(base) != 64 {
		t.Fatalf("hash %q is not hex sha256", base)
	}
	if again := envelopeHash("agp_a", "db-1", "SELECT 1", [][]byte{[]byte("x")}); again != base {
		t.Error("hash is not deterministic")
	}
	variants := []string{
		envelopeHash("agp_b", "db-1", "SELECT 1", [][]byte{[]byte("x")}),
		envelopeHash("agp_a", "db-2", "SELECT 1", [][]byte{[]byte("x")}),
		envelopeHash("agp_a", "db-1", "SELECT 2", [][]byte{[]byte("x")}),
		envelopeHash("agp_a", "db-1", "SELECT 1", [][]byte{[]byte("y")}),
		envelopeHash("agp_a", "db-1", "SELECT 1", [][]byte{nil}),
		envelopeHash("agp_a", "db-1", "SELECT 1", nil),
		// field boundaries are unambiguous
		envelopeHash("agp_a", "db-1S", "ELECT 1", [][]byte{[]byte("x")}),
	}
	for i, v := range variants {
		if v == base {
			t.Errorf("variant %d collides with the base envelope", i)
		}
	}
}

func TestSanitizedFailure(t *testing.T) {
	leak := "invalid input syntax for type integer: \"123-45-6789\""
	cases := []struct {
		code      string
		want      string
		retryable bool
	}{
		{"42501", "permission", false},
		{"57014", "statement timeout of 250ms", false},
		{"55P03", "lock", true},
		{"40001", "serialization", true},
		{"40P01", "deadlock", true},
		{"22P02", "data", false},
		{"42P01", "does not exist", false},
		{"42703", "does not exist", false},
		{"25006", "read-only", false},
		{"XX000", "SQLSTATE XX000", false},
	}
	for _, c := range cases {
		f := sanitize(&pgconn.PgError{Code: c.code, Message: leak, Detail: leak},
			250*time.Millisecond)
		if f.SQLState != c.code || f.Retryable != c.retryable {
			t.Errorf("%s: failure = %+v, want retryable %v", c.code, f, c.retryable)
		}
		if !strings.Contains(f.Message, c.want) {
			t.Errorf("%s: message %q, want it to mention %q", c.code, f.Message, c.want)
		}
		if strings.Contains(f.Message, "123-45-6789") {
			t.Errorf("%s: message leaks the server text: %q", c.code, f.Message)
		}
	}
}

func TestDecideColumn(t *testing.T) {
	cases := []struct {
		env      envbind.Env
		class    classify.Class
		unmasked bool
		want     columnAction
	}{
		{envbind.EnvProd, classify.ClassClean, false, actPass},
		{envbind.EnvProd, classify.Unclassified, false, actPass},
		{envbind.EnvProd, classify.ClassUntrusted, false, actPass},
		{envbind.EnvProd, classify.ClassPII, false, actDeny},
		{envbind.EnvProd, classify.ClassPII, true, actPass},
		{envbind.EnvStage, classify.ClassPII, false, actDeny},
		{envbind.EnvDev, classify.ClassPII, false, actMask},
		{envbind.EnvBranch, classify.ClassPII, false, actMask},
		{envbind.EnvDev, classify.ClassPII, true, actPass},
		{envbind.EnvDev, classify.ClassSecret, false, actDeny},
		{envbind.EnvBranch, classify.ClassSecret, true, actDeny},
		{envbind.Env(""), classify.ClassPII, false, actDeny}, // unknown env is prod
	}
	for _, c := range cases {
		if got := decideColumn(c.env, c.class, c.unmasked); got != c.want {
			t.Errorf("decideColumn(%q, %q, %v) = %v, want %v", c.env, c.class, c.unmasked,
				got, c.want)
		}
	}
}

func TestMaskedReferenceRefusal(t *testing.T) {
	masked := map[string]bool{"ssn": true}
	known := map[string]bool{"id": true, "name": true, "ssn": true}
	cases := []struct {
		other []string
		want  string // "" = allowed
	}{
		{nil, ""},
		{[]string{"id", "name"}, ""},
		{[]string{"ssn"}, "ssn"},
		{[]string{"id", "p"}, "p"}, // a whole-row reference
	}
	for _, c := range cases {
		got := maskedRefRefusal(masked, known, c.other)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("other %v: refusal %q, want mention of %q", c.other, got, c.want)
		}
	}
	if got := maskedRefRefusal(nil, known, []string{"anything"}); got != "" {
		t.Errorf("no masked columns: refusal %q, want none", got)
	}
}

func TestDeniedFunction(t *testing.T) {
	denied := []sqlast.QualifiedName{{Name: "set_config"}, {Name: "dblink_exec"},
		{Name: "pg_terminate_backend"}, {Name: "pg_cancel_backend"}, {Name: "lo_import"},
		{Name: "pg_read_file"}, {Name: "pg_read_binary_file"}, {Name: "pg_ls_dir"},
		{Name: "pg_advisory_lock"}, {Name: "pg_try_advisory_xact_lock"},
		{Name: "pg_sleep"}, {Name: "pg_sleep_for"}, {Schema: "sage", Name: "x"},
		{Schema: "sage_guard", Name: "y"}, {Schema: "pg_catalog", Name: "set_config"}}
	for _, n := range denied {
		if !deniedFunction(n) {
			t.Errorf("%s allowed, want denied", n)
		}
	}
	for _, n := range []sqlast.QualifiedName{{Name: "lower"}, {Name: "count"},
		{Schema: "app", Name: "sage_score"}, {Name: "lowercase_set_config"}} {
		if deniedFunction(n) {
			t.Errorf("%s denied, want allowed", n)
		}
	}
}

func TestCatalogAllowed(t *testing.T) {
	for _, n := range []string{"pg_class", "pg_namespace", "pg_attribute", "pg_index",
		"pg_constraint", "pg_type", "pg_description", "pg_tables", "pg_views"} {
		if !catalogAllowed("pg_catalog", n) {
			t.Errorf("pg_catalog.%s refused, want allowed", n)
		}
	}
	for _, n := range []string{"pg_proc", "pg_authid", "pg_roles", "pg_stats",
		"pg_stat_activity", "pg_settings", "pg_user", "pg_shadow"} {
		if catalogAllowed("pg_catalog", n) {
			t.Errorf("pg_catalog.%s allowed, want refused", n)
		}
	}
	if catalogAllowed("information_schema", "tables") {
		t.Error("information_schema allowed; R0 lists pg_catalog relations only")
	}
}

func TestEvaluateAttribution(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cur := pssInfo{Dealloc: 5, Reset: t0}
	cases := []struct {
		name   string
		in     attributionInput
		source string
		drop   bool
		reason string
	}{
		{"complete", attributionInput{Available: true, Current: cur,
			Earliest: &pssInfo{Dealloc: 5, Reset: t0}, Executed: 3, Calls: 3},
			SourcePSS, false, ""},
		{"more calls than audited", attributionInput{Available: true, Current: cur,
			Earliest: &pssInfo{Dealloc: 5, Reset: t0}, Executed: 3, Calls: 9},
			SourcePSS, false, ""},
		{"no audited calls", attributionInput{Available: true, Current: cur},
			SourcePSS, false, ""},
		{"dealloc advanced by one", attributionInput{Available: true, Current: cur,
			Earliest: &pssInfo{Dealloc: 4, Reset: t0}, Executed: 3, Calls: 3},
			SourceAudit, true, ReasonDeallocAdvanced},
		{"stats reset since", attributionInput{Available: true, Current: cur,
			Earliest: &pssInfo{Dealloc: 0, Reset: t0.Add(-time.Hour)}, Executed: 1,
			Calls: 1}, SourceAudit, true, ReasonStatsReset},
		{"entries missing", attributionInput{Available: true, Current: cur,
			Earliest: &pssInfo{Dealloc: 5, Reset: t0}, Executed: 3, Calls: 2},
			SourceAudit, true, ReasonEntriesMissing},
		{"unavailable", attributionInput{Available: false, Executed: 2},
			SourceAudit, true, ReasonPSSUnavailable},
	}
	for _, c := range cases {
		got := evaluateAttribution(c.in)
		if got.Source != c.source || got.Dropped != c.drop || got.Reason != c.reason {
			t.Errorf("%s: %+v, want source %s dropped %v reason %q", c.name, got, c.source,
				c.drop, c.reason)
		}
		if got.Complete == c.drop {
			t.Errorf("%s: Complete = %v alongside Dropped = %v", c.name, got.Complete, got.Dropped)
		}
	}
}

func TestBlockedFromVerdict(t *testing.T) {
	v := decide.Verdict{Reason: agentguard.ReasonFrozen, Step: "D1", Detail: "frozen by x",
		Fix: "unfreeze"}
	r := blocked(string(v.Reason), v.Detail, v.Fix)
	if r.Verdict != VerdictBlocked || r.ReasonCode != "agent_frozen" || r.Fix != "unfreeze" {
		t.Errorf("blocked result = %+v", r)
	}
	if r.Rows == nil || r.Columns == nil || r.Masked == nil {
		t.Error("a blocked result must carry empty, not null, rows/columns/masked")
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	if _, err := New(DefaultConfig(), Deps{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("New with no deps = %v, want ErrInvalid", err)
	}
	bad := DefaultConfig()
	bad.MaxRows = 0
	if _, err := New(bad, fakeDeps()); !errors.Is(err, ErrInvalid) {
		t.Errorf("New with a bad config = %v, want ErrInvalid", err)
	}
	b, err := New(DefaultConfig(), fakeDeps())
	if err != nil || b == nil {
		t.Fatalf("New = %v, %v", b, err)
	}
	b.Close()
}
