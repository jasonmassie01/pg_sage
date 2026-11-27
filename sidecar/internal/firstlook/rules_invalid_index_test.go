package firstlook

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// No concurrent access tests here: InvalidIndexes is a pure function over
// slices it does not retain or modify, and readIndexBuilds reads in the
// caller's transaction only.

func invalid(oid uint32, name string, cols ...int16) Index {
	x := btree(oid, 10, name, cols...)
	x.Valid = false
	return x
}

func onlyItem(t *testing.T, items []Item, object string) Item {
	t.Helper()
	var found []Item
	for _, it := range items {
		if it.Object == object {
			found = append(found, it)
		}
	}
	if len(found) != 1 {
		t.Fatalf("items for %s = %+v, want exactly one", object, items)
	}
	if found[0].Rule != RuleInvalidIndex {
		t.Fatalf("item %+v has rule %q, want %q", found[0], found[0].Rule, RuleInvalidIndex)
	}
	requireEvidence(t, found[0])
	return found[0]
}

// Leftovers of a failed REINDEX CONCURRENTLY: reindexing a _ccnew only
// makes another copy, so both suffixes get DROP. A name that merely
// contains the suffix is a genuine failure and keeps today's REINDEX.
func TestInvalidIndexLeftoversGetDrop(t *testing.T) {
	orig := btree(1, 10, "t_a", 1)
	for _, name := range []string{"t_a_ccnew", "t_a_ccnew1", "t_a_ccold", "t_a_ccold2"} {
		it := onlyItem(t, InvalidIndexes([]Index{orig, invalid(2, name, 1)}, IndexBuilds{}),
			"public."+name)
		if it.SuggestedSQL != "DROP INDEX CONCURRENTLY public."+name+";" {
			t.Errorf("%s: SQL %q, want a concurrent drop of the leftover", name, it.SuggestedSQL)
		}
		if it.Severity != SeverityWarning {
			t.Errorf("%s: severity %q, want warning", name, it.Severity)
		}
		if strings.Contains(it.Detail, "a failed CREATE INDEX") {
			t.Errorf("%s: detail %q blames a failed CREATE INDEX", name, it.Detail)
		}
	}
	it := onlyItem(t, InvalidIndexes([]Index{invalid(3, "x_ccnewer", 1)}, IndexBuilds{}),
		"public.x_ccnewer")
	if it.SuggestedSQL != "REINDEX INDEX CONCURRENTLY public.x_ccnewer;" {
		t.Fatalf("x_ccnewer: SQL %q, want the generic rebuild", it.SuggestedSQL)
	}
}

// A _ccnew whose original still exists says how to finish the rebuild; one
// whose original is gone does not name a missing index.
func TestInvalidIndexCcnewNamesTheOriginal(t *testing.T) {
	orig := btree(1, 10, "t_a", 1)
	it := onlyItem(t, InvalidIndexes([]Index{orig, invalid(2, "t_a_ccnew", 1)},
		IndexBuilds{}), "public.t_a_ccnew")
	if !strings.Contains(it.Recommendation, "REINDEX INDEX CONCURRENTLY public.t_a;") {
		t.Fatalf("recommendation %q does not re-run the reindex of public.t_a",
			it.Recommendation)
	}
	alone := onlyItem(t, InvalidIndexes([]Index{invalid(2, "t_a_ccnew", 1)}, IndexBuilds{}),
		"public.t_a_ccnew")
	if strings.Contains(alone.Recommendation, "REINDEX") {
		t.Fatalf("no original: recommendation %q names a reindex", alone.Recommendation)
	}
	if alone.SuggestedSQL != "DROP INDEX CONCURRENTLY public.t_a_ccnew;" {
		t.Fatalf("no original: SQL %q", alone.SuggestedSQL)
	}
}

func uniqueInvalid(oid uint32, name string, keys []string, cols ...int16) Index {
	x := invalid(oid, name, cols...)
	x.Unique = true
	x.KeyNames = keys
	return x
}

// An invalid unique index fails REINDEX again until the duplicates are
// gone: the item looks for them first, read-only and bounded.
func TestInvalidUniqueIndexFindsDuplicatesFirst(t *testing.T) {
	x := uniqueInvalid(2, "t_uq", []string{"a", "order"}, 1, 2)
	it := onlyItem(t, InvalidIndexes([]Index{x}, IndexBuilds{}), "public.t_uq")
	want := `SELECT a, "order", count(*) AS copies FROM public.t WHERE a IS NOT NULL AND ` +
		`"order" IS NOT NULL GROUP BY a, "order" HAVING count(*) > 1 ORDER BY count(*) DESC ` +
		`LIMIT 20;`
	if it.SuggestedSQL != want {
		t.Fatalf("SQL\n got %q\nwant %q", it.SuggestedSQL, want)
	}
	if strings.HasPrefix(it.SuggestedSQL, "REINDEX") {
		t.Fatalf("SQL %q starts with a bare REINDEX", it.SuggestedSQL)
	}
	if !strings.Contains(it.Recommendation, "duplicate") ||
		!strings.Contains(it.Recommendation, "REINDEX INDEX CONCURRENTLY public.t_uq;") {
		t.Fatalf("recommendation %q does not put the duplicates before the rebuild",
			it.Recommendation)
	}
	if it.Severity != SeverityWarning {
		t.Fatalf("severity %q, want warning", it.Severity)
	}
}

// A partial unique index only constrains rows of its predicate; NULLS NOT
// DISTINCT counts NULL keys as equal, so they are not filtered out.
func TestInvalidUniqueIndexPredicateAndNulls(t *testing.T) {
	x := uniqueInvalid(2, "t_uq", []string{"a"}, 1)
	x.Predicate = "(b > 0)"
	x.NullsNotDistinct = true
	it := onlyItem(t, InvalidIndexes([]Index{x}, IndexBuilds{}), "public.t_uq")
	want := "SELECT a, count(*) AS copies FROM public.t WHERE ((b > 0)) GROUP BY a " +
		"HAVING count(*) > 1 ORDER BY count(*) DESC LIMIT 20;"
	if it.SuggestedSQL != want {
		t.Fatalf("SQL\n got %q\nwant %q", it.SuggestedSQL, want)
	}
}

// Expression keys, or keys pg_sage could not name, get no SQL: a guessed
// query would group by the wrong thing.
func TestInvalidUniqueIndexWithoutPlainKeysGetsNoSQL(t *testing.T) {
	expr := uniqueInvalid(2, "t_expr", []string{""}, 0)
	expr.Expressions = "lower(b)"
	mixed := uniqueInvalid(3, "t_mixed", []string{"a", ""}, 1, 0)
	mixed.Expressions = "lower(b)"
	unread := uniqueInvalid(4, "t_unread", nil, 1)
	for _, x := range []Index{expr, mixed, unread} {
		it := onlyItem(t, InvalidIndexes([]Index{x}, IndexBuilds{}), x.object())
		if it.SuggestedSQL != "" {
			t.Errorf("%s: SQL %q, want none", x.Name, it.SuggestedSQL)
		}
		if !strings.Contains(it.Recommendation, "duplicate") {
			t.Errorf("%s: recommendation %q does not mention duplicates", x.Name,
				it.Recommendation)
		}
	}
}

// A build in progress is not a failure: info, no SQL, and the progress row
// is cited.
func TestInvalidIndexBuildInProgressIsInfo(t *testing.T) {
	x := invalid(2, "t_new", 1)
	b := IndexBuilds{Builds: []IndexBuild{{TableOID: 10, IndexOID: 2,
		Command: "CREATE INDEX CONCURRENTLY", Phase: "waiting for old snapshots"}}}
	it := onlyItem(t, InvalidIndexes([]Index{x}, b), "public.t_new")
	if it.Severity != SeverityInfo || it.SuggestedSQL != "" {
		t.Fatalf("in progress: severity %q SQL %q, want info and no SQL", it.Severity,
			it.SuggestedSQL)
	}
	if !strings.Contains(it.Title, "in progress") ||
		strings.Contains(it.Detail, "failed") {
		t.Fatalf("in progress: title %q detail %q", it.Title, it.Detail)
	}
	if it.Evidence[0].Source != "pg_index" || len(it.Evidence) != 2 ||
		it.Evidence[1].Source != "pg_stat_progress_create_index" ||
		!strings.Contains(it.Evidence[1].Detail, "waiting for old snapshots") {
		t.Fatalf("in progress: evidence %+v", it.Evidence)
	}
}

// Even a _ccnew or an invalid unique index is left alone while it builds.
func TestInvalidIndexBuildInProgressWinsOverLeftoverAndUnique(t *testing.T) {
	cc := invalid(2, "t_a_ccnew", 1)
	uq := uniqueInvalid(3, "t_uq", []string{"a"}, 1)
	b := IndexBuilds{Builds: []IndexBuild{
		{TableOID: 10, IndexOID: 1, Command: "REINDEX CONCURRENTLY", Phase: "building index"},
		{TableOID: 10, IndexOID: 3, Command: "CREATE INDEX CONCURRENTLY", Phase: "x"}}}
	for _, it := range InvalidIndexes([]Index{btree(1, 10, "t_a", 1), cc, uq}, b) {
		if it.Severity != SeverityInfo || it.SuggestedSQL != "" {
			t.Errorf("%s: severity %q SQL %q, want info and no SQL", it.Object, it.Severity,
				it.SuggestedSQL)
		}
	}
}

// Boundary: index_relid 0 (a CREATE INDEX that has not named its index)
// matches at table level; a build of another index on the same table does
// not, unless it is a REINDEX CONCURRENTLY, whose index_relid is the
// original while its _ccnew copies are invalid.
func TestInvalidIndexBuildMatching(t *testing.T) {
	x := invalid(2, "t_new", 1)
	cases := []struct {
		name  string
		build IndexBuild
		live  bool
	}{
		{"index_relid 0 on the table", IndexBuild{TableOID: 10, Command: "CREATE INDEX"}, true},
		{"index_relid 0 on another table", IndexBuild{TableOID: 11}, false},
		{"another index on the table", IndexBuild{TableOID: 10, IndexOID: 7,
			Command: "CREATE INDEX CONCURRENTLY"}, false},
		{"reindex concurrently on the table", IndexBuild{TableOID: 10, IndexOID: 7,
			Command: "REINDEX CONCURRENTLY"}, true},
		{"same index", IndexBuild{TableOID: 10, IndexOID: 2}, true},
	}
	for _, c := range cases {
		it := onlyItem(t, InvalidIndexes([]Index{x}, IndexBuilds{Builds: []IndexBuild{c.build}}),
			"public.t_new")
		if got := it.Severity == SeverityInfo; got != c.live {
			t.Errorf("%s: severity %q, want live=%t", c.name, it.Severity, c.live)
		}
	}
}

// A nil build set keeps today's text for plain names.
func TestInvalidIndexNilBuildsKeepsTodaysText(t *testing.T) {
	it := onlyItem(t, InvalidIndexes([]Index{invalid(2, "t_bad", 1)}, IndexBuilds{}),
		"public.t_bad")
	if it.SuggestedSQL != "REINDEX INDEX CONCURRENTLY public.t_bad;" ||
		it.Severity != SeverityWarning || it.Caveat != "" ||
		it.Title != "Invalid index public.t_bad" ||
		!strings.Contains(it.Detail, "a failed CREATE INDEX CONCURRENTLY or REINDEX") ||
		it.Recommendation != "Rebuild it if it is needed, otherwise drop it." {
		t.Fatalf("plain invalid index changed: %+v", it)
	}
}

// When some builds cannot be seen, the items say so; the in-progress ones
// need no caveat.
func TestInvalidIndexUnseenBuildsAddACaveat(t *testing.T) {
	b := IndexBuilds{Unseen: "2 builds of other roles are hidden",
		Builds: []IndexBuild{{TableOID: 10, IndexOID: 3}}}
	items := InvalidIndexes([]Index{invalid(2, "t_bad", 1), invalid(3, "t_new", 1)}, b)
	bad := onlyItem(t, items, "public.t_bad")
	if !strings.Contains(bad.Caveat, "2 builds of other roles are hidden") ||
		!strings.Contains(bad.Caveat, "pg_stat_progress_create_index") {
		t.Fatalf("caveat %q does not say which builds are unseen", bad.Caveat)
	}
	if bad.Severity != SeverityWarning {
		t.Fatalf("unseen builds changed severity to %q", bad.Severity)
	}
	if live := onlyItem(t, items, "public.t_new"); live.Caveat != "" {
		t.Fatalf("in-progress item has caveat %q", live.Caveat)
	}
}

// errTx fails the savepoint, or the query inside it.
type errTx struct {
	pgx.Tx
	beginErr, queryErr error
}

func (e errTx) Begin(context.Context) (pgx.Tx, error) {
	if e.beginErr != nil {
		return nil, e.beginErr
	}
	return errTx{queryErr: e.queryErr}, nil
}

func (e errTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, e.queryErr
}

func (e errTx) Rollback(context.Context) error { return nil }

// Error propagation: a progress read error becomes the caveat, and the
// index step's checks stay OK or finding, never degraded.
func TestReadIndexBuildsErrorBecomesCaveat(t *testing.T) {
	denied := errors.New("permission denied for view pg_stat_progress_create_index")
	for _, tx := range []errTx{{beginErr: errors.New("savepoint refused")},
		{queryErr: denied}} {
		b := readIndexBuilds(context.Background(), tx)
		if len(b.Builds) != 0 || b.Unseen == "" {
			t.Fatalf("failed read gave %+v, want no builds and a reason", b)
		}
		p := capPass(50)
		out := p.evalIndexes(invalidIndexes(1), nil, b, false)
		c := checkFor(out[0], "")
		if c.Rule != RuleInvalidIndex || c.Status != CheckFinding {
			t.Fatalf("invalid-index check = %+v, want a finding", c)
		}
		if it := p.report.Items[0]; !strings.Contains(it.Caveat, b.Unseen) {
			t.Fatalf("item caveat %q does not carry %q", it.Caveat, b.Unseen)
		}
		empty := checkFor(capPass(50).evalIndexes(nil, nil, b, false)[0], "")
		if empty.Status != CheckOK {
			t.Fatalf("no invalid indexes with a failed read: %+v, want OK", empty)
		}
	}
	b := readIndexBuilds(context.Background(), errTx{queryErr: denied})
	if !strings.Contains(b.Unseen, "permission denied") {
		t.Fatalf("reason %q drops the cause", b.Unseen)
	}
}
