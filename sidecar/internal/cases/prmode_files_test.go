package cases

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/codehost/codehosttest"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: naming and literal editing are pure functions.

func TestNextMigrationNameFollowsTheNewestFile(t *testing.T) {
	for name, tc := range map[string]struct {
		files []string
		want  []string
	}{
		"timestamp": {[]string{"20250101000000_init.sql", "20260301120000_add.sql"},
			[]string{"20261117093015_pg_sage_fix_it.sql"}},
		"sequence keeps the width": {[]string{"0001_init.sql", "0009_more.sql", "0010_x.sql"},
			[]string{"0011_pg_sage_fix_it.sql"}},
		"sequence numeric not lexical": {[]string{"9_a.sql", "10_b.sql"},
			[]string{"11_pg_sage_fix_it.sql"}},
		"flyway": {[]string{"V1__init.sql", "V12__orders.sql", "V2__x.sql"},
			[]string{"V13__pg_sage_fix_it.sql"}},
		"up and down": {[]string{"000003_x.down.sql", "000003_x.up.sql", "000002_a.up.sql"},
			[]string{"000004_pg_sage_fix_it.up.sql", "000004_pg_sage_fix_it.down.sql"}},
		"other files ignored": {[]string{"README.md", "0002_b.sql", "schema.rb"},
			[]string{"0003_pg_sage_fix_it.sql"}},
	} {
		got, err := nextMigrationNames(tc.files, "fix it", prNow)
		require.NoError(t, err, name)
		require.Equal(t, tc.want, got, name)
	}
}

func TestNextMigrationNameRefusesAnUnknownPattern(t *testing.T) {
	for name, files := range map[string][]string{
		"empty":       nil,
		"no pattern":  {"README.md", "init.sql"},
		"mixed kinds": {"V1__a.sql", "0002_b.sql"},
	} {
		_, err := nextMigrationNames(files, "x", prNow)
		require.ErrorIs(t, err, errNoPR, name)
		require.ErrorContains(t, err, "naming pattern", name)
	}
}

func TestMigrationSlug(t *testing.T) {
	require.Equal(t, "pg_sage_add_index_on_orders_created_at",
		migrationSlug("Add index on orders(created_at)!"))
	require.Equal(t, "pg_sage_change", migrationSlug("  ¿¿ "))
	long := migrationSlug(strings.Repeat("abc ", 40))
	require.True(t, len(long) <= len("pg_sage_")+48, "slug %q", long)
	require.False(t, strings.HasSuffix(long, "_"))
}

func TestMigrationFilesWriteDownOnlyForUpDownPairs(t *testing.T) {
	c := migrationCandidate()
	files := migrationFiles("db/m", []string{"0005_pg_sage_x.up.sql", "0005_pg_sage_x.down.sql"},
		c.MigrationSQL, c.RollbackSQL)
	require.Equal(t, 2, len(files))
	require.Equal(t, "db/m/0005_pg_sage_x.down.sql", files[1].Path)
	require.Equal(t, c.RollbackSQL+"\n", files[1].Content)
	files = migrationFiles("db/m", []string{"0005_pg_sage_x.up.sql", "0005_pg_sage_x.down.sql"},
		c.MigrationSQL, "")
	require.Contains(t, files[1].Content, "no rollback")
}

const appFile = `package store

const listOrders = "SELECT id FROM orders WHERE status = 'open' ORDER BY id"

func List() string { return listOrders }
`

func TestReplaceLiteralEditsTheOneExactOccurrence(t *testing.T) {
	got, err := replaceLiteral(appFile, 3,
		"SELECT id FROM orders WHERE status = 'open' ORDER BY id",
		"SELECT id FROM orders WHERE status = 'open' ORDER BY id LIMIT 500")
	require.NoError(t, err)
	require.Contains(t, got, `"SELECT id FROM orders WHERE status = 'open' ORDER BY id LIMIT 500"`)
	require.Equal(t, strings.Count(appFile, "\n"), strings.Count(got, "\n"))
}

func TestReplaceLiteralRefusesUnsafeEdits(t *testing.T) {
	stmt := "SELECT id FROM orders WHERE status = 'open' ORDER BY id"
	for name, tc := range map[string]struct {
		content, rewrite string
		line             int
		want             string
	}{
		"other line": {appFile, stmt + " LIMIT 5", 5, "not at line 5"},
		"twice": {appFile + "\nconst again = \"" + stmt + "\"\n", stmt + " LIMIT 5", 3,
			"2 times"},
		"absent":       {"package x\n", stmt + " LIMIT 5", 1, "not found verbatim"},
		"new quote":    {appFile, stmt + ` AND note = "x"`, 3, "quote"},
		"backslash":    {appFile, stmt + ` AND a ~ '\d'`, 3, "backslash"},
		"same text":    {appFile, stmt, 3, "same as the statement"},
		"empty":        {appFile, "  ", 3, "empty"},
		"line zero":    {appFile, stmt + " LIMIT 5", 0, "line"},
		"new newline":  {appFile, stmt + "\nLIMIT 5", 3, "newline"},
		"new backtick": {appFile, stmt + " -- `x`", 3, "quote"},
	} {
		_, err := replaceLiteral(tc.content, tc.line, stmt, tc.rewrite)
		require.ErrorIs(t, err, errNoPR, name)
		require.ErrorContains(t, err, tc.want, name)
	}
}

func rewriteCandidate() PRCandidate {
	return PRCandidate{FindingID: 52, Database: "shop", Kind: PRKindRewrite, QueryID: 77,
		Title: "Bound the open orders list", RiskTier: "moderate",
		Why:            "The list reads every open order on each page view.",
		EvidenceIDs:    []string{"finding:52"},
		OriginalSQL:    "SELECT id FROM orders WHERE status = 'open' ORDER BY id",
		RewrittenSQL:   "SELECT id FROM orders WHERE status = 'open' ORDER BY id LIMIT 500",
		RewriteChecked: true}
}

func rewriteRepo() *codehosttest.Fake {
	host := codehosttest.New()
	host.AddFile(prRepo, "app/store/orders.go", appFile)
	return host
}

func TestPRModeRewriteAtAnExactCallSiteEditsTheLiteral(t *testing.T) {
	host, rec := rewriteRepo(), newMemRecords()
	sites := stubSites{site: PRCallSite{Path: "app/store/orders.go", Line: 3,
		Confidence: "exact"}}
	out, err := newTestPRMode(host, rec, nil, sites).Run(context.Background(),
		rewriteCandidate())
	require.NoError(t, err)
	require.Equal(t, PROpened, out.Status)
	pr := host.Opened()[0]
	require.Equal(t, 1, len(pr.Files))
	require.Equal(t, "app/store/orders.go", pr.Files[0].Path)
	require.Contains(t, pr.Files[0].Content, "ORDER BY id LIMIT 500\"")
	require.Contains(t, pr.Body, "app/store/orders.go:3 (exact)")
	require.Contains(t, pr.Body, "application_rollback")
}

func TestPRModeRewriteWithALikelyCallSiteOpensNoPR(t *testing.T) {
	host, rec := rewriteRepo(), newMemRecords()
	sites := stubSites{site: PRCallSite{Path: "app/store/orders.go", Line: 3,
		Confidence: "likely", Note: "matched by route"}}
	out, err := newTestPRMode(host, rec, nil, sites).Run(context.Background(),
		rewriteCandidate())
	require.NoError(t, err)
	require.Equal(t, PRNotOpened, out.Status)
	require.Contains(t, out.Note, "the call site is likely, not exact (matched by route)")
	require.Contains(t, out.Note, "the rewrite stays as text")
	require.Empty(t, host.Opened())
}

func TestPRModeRewriteRefusals(t *testing.T) {
	exact := stubSites{site: PRCallSite{Path: "app/store/orders.go", Line: 3,
		Confidence: "exact"}}
	for name, tc := range map[string]struct {
		mutate func(*PRCandidate)
		sites  CallSiteFinder
		want   string
	}{
		"not checked": {func(c *PRCandidate) { c.RewriteChecked = false }, exact,
			"no equivalence check passed"},
		"no finder":  {func(*PRCandidate) {}, nil, "no call-site lookup"},
		"no queryid": {func(c *PRCandidate) { c.QueryID = 0 }, exact, "queryid"},
		"lookup failed": {func(*PRCandidate) {}, stubSites{err: errBoom},
			"call-site lookup failed: boom"},
		"not in file": {func(c *PRCandidate) { c.OriginalSQL = "SELECT 1" }, exact,
			"not found verbatim"},
	} {
		host, rec := rewriteRepo(), newMemRecords()
		c := rewriteCandidate()
		tc.mutate(&c)
		out, _ := newTestPRMode(host, rec, nil, tc.sites).Run(context.Background(), c)
		require.Equal(t, PRNotOpened, out.Status, name)
		require.Contains(t, out.Note, tc.want, name)
		require.Empty(t, host.Opened(), name)
	}
}
