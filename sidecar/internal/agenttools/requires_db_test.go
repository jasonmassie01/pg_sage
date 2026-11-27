package agenttools

import (
	"errors"
	"sort"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// mcp-09: RequiredExtensions names the extensions without which a tool
// cannot work, and MCP hides the tool where they are missing. This keeps
// the table honest on a real database without pg_stat_statements: every
// tool the table does not gate answers a minimal valid call with something
// other than ErrUnavailable, and every tool it gates does fail that way.

// No concurrent access test: RequiredExtensions is a read-only table and
// each call below is independent.

// agentToolCall is a minimal valid call of one coding-agent tool.
type agentToolCall func(f *fixture, tools *Tools) error

func agentToolCalls(findingID int64) map[string]agentToolCall {
	return map[string]agentToolCall{
		"top_queries": func(f *fixture, tools *Tools) error {
			_, err := tools.TopQueries(f.ctx, TopQueriesRequest{})
			return err
		},
		"explain_query": func(f *fixture, tools *Tools) error {
			_, err := tools.ExplainQuery(f.ctx, ExplainRequest{Query: "SELECT 1"})
			return err
		},
		"whatif_index": func(f *fixture, tools *Tools) error {
			_, err := tools.WhatIfIndex(f.ctx, WhatIfRequest{
				DDL: "CREATE INDEX ON " + f.q("t") + " (b)", QueryIDs: []QueryID{42}})
			return err
		},
		"lint_migration": func(f *fixture, tools *Tools) error {
			_, err := tools.LintMigration(f.ctx, LintRequest{
				SQL: "ALTER TABLE " + f.q("t") + " ADD COLUMN c int"})
			return err
		},
		"query_sources": func(f *fixture, tools *Tools) error {
			_, err := tools.QuerySources(f.ctx, SourcesRequest{})
			return err
		},
		"mark_object": func(f *fixture, tools *Tools) error {
			_, err := tools.MarkObject(f.ctx, MarkRequest{Kind: "table", Subject: f.q("t"),
				Mark: "exempt", Evidence: "managed by the billing team"}, markActor)
			return err
		},
		"get_source_fix_packet": func(f *fixture, tools *Tools) error {
			_, err := tools.SourceFixPacket(f.ctx, findingID)
			return err
		},
		"report_source_fix": func(f *fixture, tools *Tools) error {
			_, err := tools.ReportSourceFix(f.ctx, ReportRequest{FindingID: findingID,
				Stage: "pr_opened", PRURL: prURL, Commit: "abc1234"}, reportActor)
			return err
		},
	}
}

// noStatementsFixture is a fixture on a fresh database whose
// pg_stat_statements extension is dropped; with hypopg it also has HypoPG.
func noStatementsFixture(t *testing.T, hypopg bool) *fixture {
	t.Helper()
	base := newFixture(t)
	pool := extraDatabase(t, base.ctx, "nopss")
	f := &fixture{t: t, ctx: base.ctx, pool: pool, schema: uniqueName("np_")}
	f.exec("DROP EXTENSION IF EXISTS pg_stat_statements", "DROP EXTENSION IF EXISTS hypopg",
		"CREATE SCHEMA "+f.schema)
	if hypopg {
		var available bool
		f.scalar(&available, `SELECT EXISTS (SELECT 1 FROM pg_available_extensions
			WHERE name = 'hypopg')`)
		if !available {
			t.Skip("HypoPG server extension unavailable; the HypoPG variant is not verified")
		}
		f.exec("CREATE EXTENSION hypopg")
	}
	var installed bool
	f.scalar(&installed, `SELECT EXISTS (SELECT 1 FROM pg_extension
		WHERE extname = 'pg_stat_statements')`)
	require.False(t, installed, "the fixture must lack pg_stat_statements")
	sourceTable(f)
	return f
}

// callAll runs every tool's minimal call and returns each error.
func callAll(t *testing.T, hypopg bool) map[string]error {
	f := noStatementsFixture(t, hypopg)
	calls := agentToolCalls(indexFinding(t, f, bigQueryID))
	tools := New(f.pool, Options{Excluded: testExcluded})
	names := make([]string, 0, len(calls))
	for name := range calls {
		names = append(names, name)
	}
	sort.Strings(names)
	out := map[string]error{}
	for _, name := range names {
		out[name] = calls[name](f, tools)
	}
	for name := range RequiredExtensions {
		require.Contains(t, names, name, "RequiredExtensions names an unknown tool")
	}
	return out
}

// requireUngatedWork fails when a tool MCP keeps listing without
// pg_stat_statements is unavailable (or fails at all) there.
func requireUngatedWork(t *testing.T, errs map[string]error) {
	t.Helper()
	for name, err := range errs {
		if RequiredExtensions[name] != "" {
			continue
		}
		require.False(t, errors.Is(err, ErrUnavailable),
			"%s is listed without pg_stat_statements but is unavailable: %v", name, err)
		require.NoError(t, err, name)
	}
}

func TestRequiredExtensionsHoldWithoutStatements(t *testing.T) {
	errs := callAll(t, false)
	requireUngatedWork(t, errs)
	require.True(t, errors.Is(errs["top_queries"], ErrUnavailable),
		"top_queries reads only pg_stat_statements: %v", errs["top_queries"])
}

// With HypoPG installed, whatif_index reads its query ids from
// pg_stat_statements, so without that extension it cannot run at all.
func TestRequiredExtensionsHoldWithoutStatementsWithHypoPG(t *testing.T) {
	errs := callAll(t, true)
	requireUngatedWork(t, errs)
	require.True(t, errors.Is(errs["top_queries"], ErrUnavailable), "%v", errs["top_queries"])
	require.True(t, errors.Is(errs["whatif_index"], ErrUnavailable),
		"whatif_index measures pg_stat_statements queries: %v", errs["whatif_index"])
}

func TestRequiredExtensionsTable(t *testing.T) {
	require.Equal(t, map[string]string{"top_queries": "pg_stat_statements",
		"whatif_index": "pg_stat_statements"}, RequiredExtensions)
	require.Len(t, agentToolCalls(1), 8, "every coding-agent tool has a minimal call")
}
