package facts

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
)

// CategoryTestFixtureCleanup is one confirmed test-fixture fact's cleanup
// batch: the matching schemas and the script that drops them, for an
// operator to review and run once. pg_sage never drops a schema itself.
const CategoryTestFixtureCleanup = "test_fixture_cleanup"

// maxCleanupSchemas bounds one cleanup batch.
const maxCleanupSchemas = 500

// FilterSnapshot removes the tables and indexes of schemas a confirmed
// test-fixture fact covers from snap, so no rule, model or budget spends
// anything on them, and returns those schemas (sorted).
func FilterSnapshot(facts []Fact, now time.Time, snap *collector.Snapshot) []string {
	if snap == nil {
		return nil
	}
	patterns := fixturePatterns(facts, now)
	if len(patterns) == 0 {
		return nil
	}
	excluded := map[string]bool{}
	isFixture := func(schema string) bool {
		if v, seen := excluded[schema]; seen {
			return v
		}
		excluded[schema] = matchesAny(patterns, schema)
		return excluded[schema]
	}
	tables := snap.Tables[:0]
	for _, t := range snap.Tables {
		if !isFixture(t.SchemaName) {
			tables = append(tables, t)
		}
	}
	snap.Tables = tables
	indexes := snap.Indexes[:0]
	for _, i := range snap.Indexes {
		if !isFixture(i.SchemaName) {
			indexes = append(indexes, i)
		}
	}
	snap.Indexes = indexes
	var out []string
	for schema, fixture := range excluded {
		if fixture {
			out = append(out, schema)
		}
	}
	sort.Strings(out)
	return out
}

func fixturePatterns(facts []Fact, now time.Time) []Pattern {
	var out []Pattern
	for _, f := range facts {
		if f.Type != TypeTestFixture || !f.binds(now) {
			continue
		}
		if p, err := ParsePattern(f.Kind, f.Subject); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func matchesAny(patterns []Pattern, schema string) bool {
	for _, p := range patterns {
		if globMatch(p.Schema, schema) {
			return true
		}
	}
	return false
}

// ApplyFindings applies the confirmed facts to a cycle's findings: findings
// on test fixtures are dropped, executable changes a fact binds are
// redirected (their SQL becomes a source-fix packet), and each test-fixture
// fact with matching schemas (excluded) gets one cleanup finding. It
// returns the categories it evaluated.
func ApplyFindings(facts []Fact, findings []analyzer.Finding, excluded []string,
	now time.Time) ([]analyzer.Finding, []string) {
	patterns := fixturePatterns(facts, now)
	out := make([]analyzer.Finding, 0, len(findings)+len(patterns))
	for _, f := range findings {
		if f.Category != CategoryTestFixtureCleanup && onFixture(patterns, f) {
			continue
		}
		out = append(out, redirect(facts, f, now))
	}
	for _, fact := range facts {
		if fact.Type != TypeTestFixture || !fact.binds(now) {
			continue
		}
		if c, ok := cleanupFinding(fact, excluded); ok {
			out = append(out, c)
		}
	}
	return out, []string{CategoryTestFixtureCleanup}
}

func findingRef(f analyzer.Finding) (ObjectRef, bool) {
	ref, err := ParseObjectRef(f.ObjectIdentifier)
	return ref, err == nil
}

func onFixture(patterns []Pattern, f analyzer.Finding) bool {
	ref, ok := findingRef(f)
	return ok && ref.Kind != KindSlot && ref.Schema != "" && matchesAny(patterns, ref.Schema)
}

// redirect turns a finding's executable change into a source-fix packet
// when a confirmed fact binds it (as an operator's own request would be
// bound: windows and fixtures do not redirect).
func redirect(facts []Fact, f analyzer.Finding, now time.Time) analyzer.Finding {
	if strings.TrimSpace(f.RecommendedSQL) == "" {
		return f
	}
	req := Request{SQL: f.RecommendedSQL, Targets: []string{f.ObjectIdentifier},
		OperatorApproved: true, Now: now}
	refs := withTableHint(ResolveRefs(req), f)
	bindings := Bind(facts, refs, req)
	if len(bindings) == 0 {
		return f
	}
	fixes := make([]SourceFix, 0, len(bindings))
	ids := make([]int64, 0, len(bindings))
	for _, b := range bindings {
		fixes = append(fixes, BuildSourceFix(b, f.RecommendedSQL, f.RollbackSQL))
		ids = append(ids, b.Fact.ID)
	}
	detail := make(map[string]any, len(f.Detail)+3)
	for k, v := range f.Detail {
		detail[k] = v
	}
	detail["source_fix"], detail["fact_bindings"], detail["bound_by_facts"] = fixes[0], fixes,
		ids
	f.Detail = detail
	f.Recommendation = strings.TrimSpace(f.Recommendation + " " + fixes[0].Summary)
	f.RecommendedSQL, f.RollbackSQL, f.ActionRisk = "", "", ""
	return f
}

// withTableHint gives index references the table a finding names in its
// detail ("table"), so a table fact binds the table's indexes.
func withTableHint(refs []ObjectRef, f analyzer.Finding) []ObjectRef {
	table, _ := f.Detail["table"].(string)
	hint, err := ParseObjectRef(table)
	if table == "" || err != nil || hint.Kind == KindSlot {
		return refs
	}
	for i, r := range refs {
		if (r.Kind == KindIndex || r.Kind == KindRelation) && r.TableName == "" {
			refs[i].TableSchema, refs[i].TableName = hint.Schema, hint.Name
		}
	}
	return refs
}

func cleanupFinding(fact Fact, excluded []string) (analyzer.Finding, bool) {
	p, err := ParsePattern(fact.Kind, fact.Subject)
	if err != nil {
		return analyzer.Finding{}, false
	}
	var schemas []string
	for _, s := range excluded {
		if globMatch(p.Schema, s) {
			schemas = append(schemas, s)
		}
	}
	if len(schemas) == 0 {
		return analyzer.Finding{}, false
	}
	return analyzer.Finding{
		Category: CategoryTestFixtureCleanup, Severity: "info", ObjectType: "schema",
		ObjectIdentifier: fact.Subject,
		Title: fmt.Sprintf("%d test-fixture schemas match %s (fact #%d): cleanup batch "+
			"ready", len(schemas), fact.Subject, fact.ID),
		Detail: map[string]any{"fact_id": fact.ID, "pattern": fact.Subject,
			"schemas": schemas, "schema_count": len(schemas),
			"cleanup_sql": cleanupSQL(schemas), "provenance": fact.Provenance()},
		Recommendation: "These schemas are confirmed test fixtures (" + fact.Provenance() +
			"): pg_sage excludes them from findings and budgets. Review the batch and " +
			"run it once to drop them; pg_sage never drops schemas itself.",
	}, true
}

// cleanupSQL is one transaction dropping the schemas (at most
// maxCleanupSchemas; the rest wait for the next batch).
func cleanupSQL(schemas []string) string {
	var b strings.Builder
	b.WriteString("BEGIN;\n")
	for i, s := range schemas {
		if i == maxCleanupSchemas {
			fmt.Fprintf(&b, "-- %d more schemas follow in the next batch\n",
				len(schemas)-maxCleanupSchemas)
			break
		}
		fmt.Fprintf(&b, "DROP SCHEMA IF EXISTS %s CASCADE;\n", pgx.Identifier{s}.Sanitize())
	}
	b.WriteString("COMMIT;\n")
	return b.String()
}

// FindingFilter applies a database's confirmed facts to the analyzer
// (analyzer.FactFilter).
type FindingFilter struct {
	source ConfirmedSource
	logFn  func(string, string, ...any)
	now    func() time.Time
}

// NewFindingFilter filters with the facts from source; logFn may be nil.
func NewFindingFilter(source ConfirmedSource, logFn func(string, string, ...any),
) *FindingFilter {
	if logFn == nil {
		logFn = func(string, string, ...any) {}
	}
	return &FindingFilter{source: source, logFn: logFn, now: time.Now}
}

// ExcludeSnapshot removes confirmed test fixtures from every snapshot and
// returns the excluded schemas of the first. Without facts it changes
// nothing.
func (f *FindingFilter) ExcludeSnapshot(ctx context.Context,
	snaps ...*collector.Snapshot) []string {
	facts, err := f.source.Confirmed(ctx)
	if err != nil {
		f.logFn("WARN", "facts: snapshot left unfiltered: %v", err)
		return nil
	}
	var excluded []string
	for i, s := range snaps {
		got := FilterSnapshot(facts, f.now(), s)
		if i == 0 {
			excluded = got
		}
	}
	return excluded
}

// ApplyFindings applies the confirmed facts to the findings. Without facts
// the findings pass unchanged and nothing is evaluated (so no earlier
// cleanup finding resolves on a read error).
func (f *FindingFilter) ApplyFindings(ctx context.Context, findings []analyzer.Finding,
	excluded []string) ([]analyzer.Finding, []string) {
	facts, err := f.source.Confirmed(ctx)
	if err != nil {
		f.logFn("WARN", "facts: findings left unfiltered: %v", err)
		return findings, nil
	}
	return ApplyFindings(facts, findings, excluded, f.now())
}
