package decide

import (
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

func TestToolClassTable(t *testing.T) {
	cases := []struct {
		tool string
		kind agentguard.ToolKind
		cap  Capability
	}{
		{"optimize_query", agentguard.ToolPropose, CapDDLAdditive},
		{"ensure_fk_indexes", agentguard.ToolPropose, CapDDLAdditive},
		{"sre_propose_action", agentguard.ToolPropose, CapMaint},
		{"sre_request_execution", agentguard.ToolPropose, CapMaint},
		{"specialist_request_remediation", agentguard.ToolPropose, CapMaint},
		{"set_maintenance_policy", agentguard.ToolPropose, CapPolicyProposal},
		{"propose_policy_change", agentguard.ToolPropose, CapPolicyProposal},
		{"sre_draft_runbook", agentguard.ToolPropose, CapPolicyProposal},
		{"sre_compile_runbook", agentguard.ToolPropose, CapPolicyProposal},
		{"propose_fact", agentguard.ToolPropose, ""},
		{"mark_object", agentguard.ToolPropose, ""},
		{"report_source_fix", agentguard.ToolPropose, ""},
		{"sre_evaluate_autonomy", agentguard.ToolPropose, ""},
		{"propose_plan", agentguard.ToolPropose, ""},
		{"list_plans", agentguard.ToolRead, ""},
		{"get_plan", agentguard.ToolRead, ""},
		{"top_queries", agentguard.ToolRead, ""},
		{"get_policy", agentguard.ToolRead, ""},
		{"upgrade_readiness", agentguard.ToolRead, ""},
		{"get_inbox", agentguard.ToolRead, ""},
		{"get_outcomes", agentguard.ToolRead, ""},
		{"get_workload_pack", agentguard.ToolRead, ""},
		{"decide_fact", KindApprove, ""},
		{"declare_table_contract", KindApprove, ""},
		{"register_consumer", KindApprove, ""},
		{"sre_downgrade_autonomy", KindApprove, ""},
		{"sre_review_investigation", KindApprove, ""},
		{"agent_query", agentguard.ToolAgent, CapRead},
	}
	for _, tc := range cases {
		kind, cap := KindFor(tc.tool), CapabilityFor(tc.tool, "")
		if kind != tc.kind || cap != tc.cap {
			t.Fatalf("%s = (%s, %q), want (%s, %q)", tc.tool, kind, cap, tc.kind, tc.cap)
		}
	}
}

func TestUnknownToolFailsClosed(t *testing.T) {
	if got := KindFor("brand_new_tool"); got != agentguard.ToolAgent {
		t.Fatalf("KindFor(unknown) = %s, want agent (every D-step applies)", got)
	}
	if got := KindFor(""); got != agentguard.ToolAgent {
		t.Fatalf("KindFor(\"\") = %s, want agent", got)
	}
	if got := CapabilityFor("brand_new_tool", "CREATE INDEX x ON t (a)"); got != "" {
		t.Fatalf("CapabilityFor(unknown) = %q, want none (D4 denies)", got)
	}
}

func TestSchemaIntentsClassifyTheirSQL(t *testing.T) {
	cases := map[string]Capability{
		"":                                    CapDDLDestructive, // unknown: worst case
		"CREATE TABLE app.t (id int)":         CapDDLAdditive,
		"ALTER TABLE app.t ADD COLUMN c text": CapDDLAdditive,
		"CREATE INDEX CONCURRENTLY i ON app.t (c)":      CapDDLAdditive,
		"CREATE INDEX i ON app.t (c)":                   CapDDLLocking,
		"ALTER TABLE app.t ALTER COLUMN c SET NOT NULL": CapDDLLocking,
		"ALTER TABLE app.t ALTER COLUMN c TYPE bigint":  CapDDLLocking,
		"DROP TABLE app.t":                              CapDDLDestructive,
		"ALTER TABLE app.t DROP COLUMN c":               CapDDLDestructive,
		"TRUNCATE app.t":                                CapDDLDestructive,
		"drop index app.i":                              CapDDLDestructive,
		"ALTER TABLE app.t DROP CONSTRAINT k":           CapDDLDestructive,
		"DROP SCHEMA app CASCADE":                       CapDDLDestructive,
		"CREATE TABLE a (id int); DROP TABLE b":         CapDDLDestructive,
		"-- DROP TABLE x\nCREATE TABLE app.t (id int)":  CapDDLAdditive,
	}
	for sql, want := range cases {
		for _, tool := range []string{"apply_migration", "request_change"} {
			if got := CapabilityFor(tool, sql); got != want {
				t.Fatalf("CapabilityFor(%s, %q) = %q, want %q", tool, sql, got, want)
			}
		}
	}
}

func TestDefaultProfilesAreTheSpecs(t *testing.T) {
	p := DefaultProfiles()
	if len(p) != 4 {
		t.Fatalf("profiles = %d, want the 4 of spec section 9", len(p))
	}
	coding, ok := p.Profile("coding-agent")
	if !ok || coding.EnvCeiling != "stage" || coding.Allows(CapDDLDestructive) ||
		!coding.Allows(CapDDLLocking) || coding.DirectLane {
		t.Fatalf("coding-agent = %+v", coding)
	}
	legacy, _ := p.Profile("legacy")
	if !legacy.Allows(CapRead) || legacy.Allows(CapWriteInsert) {
		t.Fatalf("legacy = %+v, want read only", legacy)
	}
	if _, ok := p.Profile("nope"); ok {
		t.Fatal("unknown profile must not resolve")
	}
}

func requireClass(t *testing.T, want Capability, sqls ...string) {
	t.Helper()
	for _, sql := range sqls {
		if got := ClassifyDDL(sql); got != want {
			t.Errorf("ClassifyDDL(%q) = %q, want %q", sql, got, want)
		}
	}
}

// migration-04a: the regex classifier misses these lock or rewrite forms,
// so before the allowlist they fell through to additive, the trust class
// that earns autonomy soonest.
func TestClassifyDDLMissedLockFormsAreLocking(t *testing.T) {
	forms := []string{
		"ALTER TABLE app.t SET LOGGED",
		"ALTER TABLE app.t SET UNLOGGED",
		"ALTER TABLE app.t SET ACCESS METHOD heap2",
		"ALTER TABLE app.t ADD CONSTRAINT ex EXCLUDE USING gist (r WITH &&)",
		"ALTER TABLE app.p DETACH PARTITION app.p1",
		"ALTER TABLE app.t ADD CONSTRAINT c_nn NOT NULL c",
		"ALTER TABLE app.t RENAME TO t2",
		"ALTER TABLE app.t RENAME COLUMN a TO b",
		"CREATE TRIGGER trg BEFORE INSERT ON app.t FOR EACH ROW EXECUTE FUNCTION app.f()",
		"ALTER TABLE app.t ADD COLUMN c text UNIQUE",
		"ALTER TABLE app.t ADD COLUMN c int REFERENCES app.p(id)",
		"ALTER TABLE app.t ADD COLUMN c int CHECK (c > 0)",
	}
	if len(forms) != 12 {
		t.Fatalf("the plan names 12 missed forms, table has %d", len(forms))
	}
	requireClass(t, CapDDLLocking, forms...)
}

// The allowlist: only these shapes are additive.
func TestClassifyDDLAllowlistStaysAdditive(t *testing.T) {
	requireClass(t, CapDDLAdditive,
		"CREATE INDEX CONCURRENTLY i ON app.t (c)",
		"create index concurrently if not exists i on app.t using btree (lower(c))",
		"CREATE TABLE app.t (id int)",
		"CREATE TABLE IF NOT EXISTS app.unique_users (id bigint, note text)",
		"ALTER TABLE app.t ADD COLUMN c int",
		"ALTER TABLE app.t ADD COLUMN renamed_at timestamptz",
		"ALTER TABLE IF EXISTS ONLY app.t ADD COLUMN IF NOT EXISTS c varchar(20)",
		"ALTER TABLE app.t ADD COLUMN a numeric(10, 2), ADD COLUMN b text[]",
		"ALTER TABLE app.t ADD c timestamp with time zone",
		"CREATE VIEW app.v AS SELECT id FROM app.t",
		"CREATE OR REPLACE VIEW app.v AS SELECT id FROM app.t",
		"CREATE SCHEMA app2",
		"CREATE SCHEMA IF NOT EXISTS app2 AUTHORIZATION owner_role",
		"COMMENT ON TABLE app.t IS 'rename this later; it has a trigger'",
		"CREATE SEQUENCE app.s START 10",
		"SET lock_timeout = '5s'; ALTER TABLE app.t ADD COLUMN c int",
		"BEGIN; SET LOCAL statement_timeout = 5000; CREATE TABLE app.t (id int); COMMIT",
	)
}

// ADD COLUMN ... DEFAULT: a constant or a stable default is metadata-only
// on PG 11+; a volatile one rewrites every row.
func TestClassifyDDLAddColumnDefaultBoundary(t *testing.T) {
	requireClass(t, CapDDLAdditive,
		"ALTER TABLE app.t ADD COLUMN c int DEFAULT 0",
		"ALTER TABLE app.t ADD COLUMN c timestamptz DEFAULT now()",
		"ALTER TABLE app.t ADD COLUMN c timestamptz DEFAULT current_timestamp",
		"ALTER TABLE app.t ADD COLUMN c text DEFAULT 'x'",
	)
	requireClass(t, CapDDLLocking,
		"ALTER TABLE app.t ADD COLUMN c uuid DEFAULT gen_random_uuid()",
		"ALTER TABLE app.t ADD COLUMN c bigint DEFAULT nextval('app.s')",
		"ALTER TABLE app.t ADD COLUMN c int NOT NULL DEFAULT 0",
		"ALTER TABLE app.t ADD COLUMN c int DEFAULT 0 CONSTRAINT k CHECK (c >= 0)",
		"ALTER TABLE app.t ADD COLUMN c int GENERATED ALWAYS AS (a * 2) STORED",
		"ALTER TABLE app.t ADD COLUMN c int GENERATED ALWAYS AS IDENTITY",
		"ALTER TABLE app.t ADD COLUMN c bigserial",
		"ALTER TABLE app.t ADD COLUMN c int PRIMARY KEY",
		// A domain can carry a CHECK, which rewrites; the text cannot
		// tell a domain from another type, so only built-in types pass.
		"ALTER TABLE app.t ADD COLUMN c app.positive_int",
		"ALTER TABLE app.t ADD COLUMN c DEFAULT 0",
		"ALTER TABLE app.t ADD COLUMN c int DEFAULT",
	)
}

// Only the stable functions themselves pass: a quoted or schema-qualified
// call can name any function, so it is not proven stable.
func TestClassifyDDLAddColumnDefaultCallSpelling(t *testing.T) {
	requireClass(t, CapDDLAdditive,
		"ALTER TABLE app.t ADD COLUMN c timestamptz DEFAULT pg_catalog.now()",
		"ALTER TABLE app.t ADD COLUMN c timestamptz DEFAULT now() + interval '1 day'",
	)
	requireClass(t, CapDDLLocking,
		`ALTER TABLE app.t ADD COLUMN c uuid DEFAULT "gen_random_uuid"()`,
		`ALTER TABLE app.t ADD COLUMN c uuid DEFAULT public."gen_random_uuid" ()`,
		"ALTER TABLE app.t ADD COLUMN c timestamptz DEFAULT app.now()",
		"ALTER TABLE app.t ADD COLUMN c timestamptz DEFAULT app . now()",
	)
}

// Everything outside the allowlist is locking, even with no rule match.
func TestClassifyDDLOutsideAllowlistIsLocking(t *testing.T) {
	requireClass(t, CapDDLLocking,
		"SELECT 1",
		"SELECT app.do_ddl()",
		"GRANT SELECT ON app.t TO reader",
		"SET ROLE postgres; CREATE TABLE app.t (id int)",
		"SET search_path = app; CREATE TABLE t (id int)",
		"CREATE MATERIALIZED VIEW app.mv AS SELECT * FROM app.t",
		"CREATE UNLOGGED TABLE app.t (id int)",
		"CREATE TABLE app.t (id int REFERENCES app.p (id))",
		"CREATE TABLE app.p1 PARTITION OF app.p FOR VALUES IN (1)",
		"CREATE TABLE app.child () INHERITS (app.parent)",
		"CREATE TABLE app.copy AS SELECT * FROM app.big",
		"CREATE TABLE app.copy AS TABLE app.big",
		"CREATE UNIQUE INDEX CONCURRENTLY i ON app.t (c)",
		"CREATE SCHEMA app2 CREATE TABLE t (id int)",
		"ALTER TABLE app.t ALTER COLUMN c SET DEFAULT 1",
		"ALTER TABLE app.t ADD COLUMN a int, ALTER COLUMN b SET DEFAULT 1",
		"ALTER TABLE app.p DETACH PARTITION app.p1 CONCURRENTLY",
		"ALTER TABLE app.t OWNER TO someone",
		"ALTER INDEX app.i RENAME TO j",
		// The plan's keyword floor is textual: NOT NULL in a partial
		// index predicate also counts (migration-04's AST path may narrow
		// it; it must never widen past this floor otherwise).
		"CREATE INDEX CONCURRENTLY i ON app.t (c) WHERE c IS NOT NULL",
		"COMMENT ON TRIGGER trg ON app.t IS 'x'",
		"CREATE TABLE app.t (id int); ALTER TABLE app.t SET LOGGED",
	)
}

// Empty text stays the worst case; text the scanner cannot tokenize is
// never additive (an unterminated literal could hide a statement).
func TestClassifyDDLEmptyAndUntokenizable(t *testing.T) {
	requireClass(t, CapDDLDestructive, "", "   ", "-- CREATE TABLE app.t (id int)",
		";", "/* x */ ; ;\n")
	requireClass(t, CapDDLLocking,
		"CREATE TABLE app.t (a text DEFAULT 'x); ALTER TABLE app.big SET LOGGED",
		"CREATE TABLE app.t (a text DEFAULT $$x)",
		`CREATE TABLE app."t (id int)`,
		"CREATE TABLE app.t (id int) /* unterminated",
		`CREATE TABLE app.t (a text DEFAULT E'x\')`,
	)
}

// The DROP/TRUNCATE rule still runs first: a batch that also holds a
// locking or additive statement is destructive.
func TestClassifyDDLDestructiveStillFirst(t *testing.T) {
	requireClass(t, CapDDLDestructive,
		"ALTER TABLE app.t SET LOGGED; DROP TABLE app.old",
		"CREATE INDEX CONCURRENTLY i ON app.t (c); TRUNCATE app.t",
		"ALTER TABLE app.t ADD COLUMN c int; ALTER TABLE app.t DROP COLUMN d",
	)
}

// A literal holding "--" or "/*" fooled the comment regex into eating the
// rest of the text, hiding a DROP or TRUNCATE from the destructive rule;
// the scanner's statements must still class it destructive.
func TestClassifyDDLDestructiveHiddenByLiteral(t *testing.T) {
	requireClass(t, CapDDLDestructive,
		"COMMENT ON TABLE app.t IS '-- note'; DROP TABLE app.t",
		"COMMENT ON TABLE app.t IS '/*'; DROP TABLE app.t; COMMENT ON TABLE app.u IS '*/'",
		"COMMENT ON TABLE app.t IS $$it's$$; DROP TABLE app.t; SELECT ''",
		`CREATE TABLE app.t (a text DEFAULT E'x\''); TRUNCATE app.big; SELECT ''`,
	)
	// The redacted literal itself never counts: its text is gone.
	requireClass(t, CapDDLAdditive,
		"COMMENT ON TABLE app.t IS '-- drop this table later'",
		"COMMENT ON TABLE app.t IS 'truncate /* nightly'",
	)
}

// ClassifyDDL shares compiled patterns and the classifier across
// requests; parallel callers must get the same answers.
func TestClassifyDDLConcurrentCallers(t *testing.T) {
	cases := map[string]Capability{
		"ALTER TABLE app.t SET LOGGED":             CapDDLLocking,
		"CREATE INDEX CONCURRENTLY i ON app.t (c)": CapDDLAdditive,
		"DROP TABLE app.t":                         CapDDLDestructive,
	}
	var wg sync.WaitGroup
	errs := make(chan string, 16*len(cases))
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sql, want := range cases {
				if got := ClassifyDDL(sql); got != want {
					errs <- sql + " = " + string(got)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// CREATE TABLE AS runs its query, and the query can call a function that
// does any DDL. PostgreSQL needs no space before the parenthesis, and on
// PG 16+ a vertical tab is whitespace, so neither spelling may slip
// through to additive (migration-04a review).
func TestClassifyDDLCreateTableAsSpellings(t *testing.T) {
	requireClass(t, CapDDLLocking,
		"CREATE TABLE app.copy AS(SELECT app.do_ddl())",
		"CREATE TABLE app.copy AS((SELECT 1))",
		"CREATE TABLE app.copy AS(VALUES (1))",
		"CREATE TABLE app.copy AS\vSELECT * FROM app.big",
		"CREATE TABLE app.copy\vAS TABLE app.big",
		"CREATE TABLE app.p1 PARTITION\vOF app.p FOR VALUES IN (1)",
		"ALTER TABLE app.t ADD COLUMN c int NOT\vNULL DEFAULT 0",
	)
	// AS inside a new table's own definition runs nothing.
	requireClass(t, CapDDLAdditive,
		"CREATE TABLE app.t (a int, b int GENERATED ALWAYS AS (a * 2) STORED)",
		"CREATE TABLE app.t (id int GENERATED ALWAYS AS IDENTITY)",
		"CREATE TABLE app.t\v(id int)",
	)
}
