package facts

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
)

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
// fact with matching schemas (excluded) that have a baseline in activity
// gets one cleanup finding. It returns the categories it evaluated.
func ApplyFindings(facts []Fact, findings []analyzer.Finding, excluded []string,
	activity map[string]int64, now time.Time) ([]analyzer.Finding, []string) {
	return applyFindings(facts, findings, nil, excluded, activity, now)
}

// applyFindings is ApplyFindings with each finding's refs already resolved
// (resolved[i] for findings[i]); with resolved nil they are matched as
// written.
func applyFindings(facts []Fact, findings []analyzer.Finding, resolved [][]ObjectRef,
	excluded []string, activity map[string]int64, now time.Time) ([]analyzer.Finding,
	[]string) {
	patterns := fixturePatterns(facts, now)
	out := make([]analyzer.Finding, 0, len(findings)+len(patterns))
	for i, f := range findings {
		if f.Category != CategoryTestFixtureCleanup && onFixture(patterns, f) {
			continue
		}
		refs := redirectRefs(f)
		if resolved != nil {
			refs = resolved[i]
		}
		out = append(out, redirect(facts, f, refs, now))
	}
	for _, fact := range facts {
		if fact.Type != TypeTestFixture || !fact.binds(now) {
			continue
		}
		if c, ok := cleanupFinding(fact, excluded, activity, now); ok {
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

// redirectRequest is the request a finding's executable change is bound
// as: an operator's own, so windows and fixtures do not redirect.
func redirectRequest(f analyzer.Finding, now time.Time) Request {
	return Request{SQL: f.RecommendedSQL, Targets: []string{f.ObjectIdentifier},
		OperatorApproved: true, Now: now}
}

// redirectRefs are the objects a finding's executable change touches, as
// written; nil when it has none.
func redirectRefs(f analyzer.Finding) []ObjectRef {
	if strings.TrimSpace(f.RecommendedSQL) == "" {
		return nil
	}
	return withTableHint(ResolveRefs(redirectRequest(f, time.Time{})), f)
}

// redirect turns a finding's executable change, touching refs, into a
// source-fix packet when a confirmed fact binds it.
func redirect(facts []Fact, f analyzer.Finding, refs []ObjectRef,
	now time.Time) analyzer.Finding {
	if strings.TrimSpace(f.RecommendedSQL) == "" {
		return f
	}
	bindings := Bind(facts, refs, redirectRequest(f, now))
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

// FindingFilter applies a database's confirmed facts to the analyzer
// (analyzer.FactFilter).
type FindingFilter struct {
	source   ConfirmedSource
	resolver RefResolver
	activity ActivityReader
	logFn    func(string, string, ...any)
	now      func() time.Time
}

// NewFindingFilter filters with the facts from source; logFn may be nil.
// Without WithActivity it offers no cleanup batch.
func NewFindingFilter(source ConfirmedSource, resolver RefResolver,
	logFn func(string, string, ...any)) *FindingFilter {
	if logFn == nil {
		logFn = func(string, string, ...any) {}
	}
	return &FindingFilter{source: source, resolver: resolver, logFn: logFn, now: time.Now}
}

// WithActivity reads the fixture schemas' activity baselines a cleanup
// batch embeds (read only while a fixture fact has matching schemas).
func (f *FindingFilter) WithActivity(r ActivityReader) *FindingFilter {
	f.activity = r
	return f
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
// cleanup finding resolves on a read error). Without activity baselines
// there is no cleanup batch: on a read error the earlier batch stays open
// (its blocks re-check every schema), with no reader it resolves.
func (f *FindingFilter) ApplyFindings(ctx context.Context, findings []analyzer.Finding,
	excluded []string) ([]analyzer.Finding, []string) {
	facts, err := f.source.Confirmed(ctx)
	if err != nil {
		f.logFn("WARN", "facts: findings left unfiltered: %v", err)
		return findings, nil
	}
	activity, ok := f.baselines(ctx, excluded)
	out, evaluated := applyFindings(facts, findings, f.resolveRefs(ctx, facts, findings),
		excluded, activity, f.now())
	if !ok {
		evaluated = nil
	}
	return out, evaluated
}

// baselines reads the excluded schemas' activity; ok is false on a read
// error.
func (f *FindingFilter) baselines(ctx context.Context, excluded []string) (map[string]int64,
	bool) {
	if len(excluded) == 0 {
		return nil, true
	}
	if f.activity == nil {
		f.logFn("WARN", "facts: no schema activity reader; cleanup batch withheld for "+
			"%d test-fixture schemas", len(excluded))
		return nil, true
	}
	activity, err := f.activity.SchemaActivity(ctx, excluded)
	if err != nil {
		f.logFn("WARN", "facts: cleanup batch withheld this cycle (%d test-fixture "+
			"schemas): %v", len(excluded), err)
		return nil, false
	}
	return activity, true
}

// resolveRefs reads, in one catalog query per cycle, the kinds, index
// tables and partition roots of every finding's refs, so a fact on a
// partitioned table redirects findings on its partitions (facts-10).
// Without facts or a resolver, or on a read error (logged), refs are
// matched as written (nil): the gate, which fails closed, still binds.
func (f *FindingFilter) resolveRefs(ctx context.Context, facts []Fact,
	findings []analyzer.Finding) [][]ObjectRef {
	if f.resolver == nil || len(facts) == 0 {
		return nil
	}
	per := make([][]ObjectRef, len(findings))
	var all []ObjectRef
	for i, fd := range findings {
		per[i] = redirectRefs(fd)
		all = append(all, per[i]...)
	}
	if !needsResolution(all) {
		return nil
	}
	got, err := resolveCatalog(ctx, f.resolver, all)
	if err != nil {
		f.logFn("WARN", "facts: refs of %d findings matched as written (partitions not "+
			"covered by their parent's facts this cycle): %v", len(findings), err)
		return nil
	}
	for i := range per {
		n := len(per[i])
		per[i], got = got[:n:n], got[n:]
	}
	return per
}
