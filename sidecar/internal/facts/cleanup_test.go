package facts

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// The cleanup batch is a script an operator runs in psql autocommit: one
// DO block per schema, each its own transaction, each re-checking the
// schema's activity and locks before it drops anything (facts-06).

// No concurrent access tests here: cleanupSQL and ApplyFindings are pure,
// and a FindingFilter serves one analyzer goroutine; the live tests run
// the script against concurrent sessions.

// fakeActivity is an ActivityReader with fixed counters.
type fakeActivity struct {
	counts map[string]int64
	err    error
	calls  int
	asked  []string
}

func (f *fakeActivity) SchemaActivity(_ context.Context, schemas []string) (map[string]int64,
	error) {
	f.calls++
	f.asked = append([]string(nil), schemas...)
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]int64{}
	for _, s := range schemas {
		if v, ok := f.counts[s]; ok {
			out[s] = v
		}
	}
	return out, nil
}

// dropBlocks are the script's DO blocks: tag, body.
var dropBlockPattern = regexp.MustCompile(`(?s)\nDO (\$sage\d*\$)\n(.*?)\n(\$sage\d*\$);\n`)

func dropBlocks(t *testing.T, script string) [][2]string {
	t.Helper()
	var out [][2]string
	for _, m := range dropBlockPattern.FindAllStringSubmatch(script, -1) {
		if m[1] != m[3] {
			t.Fatalf("block opened with %s closed with %s", m[1], m[3])
		}
		out = append(out, [2]string{m[1], m[2]})
	}
	return out
}

func schemaRange(n int) ([]string, map[string]int64) {
	names := make([]string, n)
	base := make(map[string]int64, n)
	for i := range names {
		names[i] = fmt.Sprintf("test_run_%04d", i)
		base[names[i]] = int64(i * 3)
	}
	return names, base
}

func countLinesWithPrefix(script, prefix string) int {
	n := 0
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

func TestCleanupSQLCapsTheBatchAtFiveHundredDropBlocks(t *testing.T) {
	names, base := schemaRange(501)
	script := cleanupSQL(7, "test_run_*", names, base, bindNow)
	if got := countLinesWithPrefix(script, "DO "); got != maxCleanupSchemas {
		t.Fatalf("%d DO blocks, want %d", got, maxCleanupSchemas)
	}
	blocks := dropBlocks(t, script)
	if len(blocks) != maxCleanupSchemas {
		t.Fatalf("%d well-formed blocks", len(blocks))
	}
	for i, b := range blocks {
		wantName := fmt.Sprintf("v_schema text := '%s';", names[i])
		wantBase := fmt.Sprintf("v_baseline bigint := %d;", base[names[i]])
		if !strings.Contains(b[1], wantName) || !strings.Contains(b[1], wantBase) {
			t.Fatalf("block %d lacks %q / %q:\n%s", i, wantName, wantBase, b[1])
		}
	}
	if strings.Contains(script, names[500]) {
		t.Fatal("the 501st schema is in the batch")
	}
	for _, want := range []string{"-- 1 more schemas follow in the next batch",
		"SET lock_timeout = '5s';", "fact #7", "test_run_*",
		bindNow.UTC().Format(time.RFC3339), "never wrap it in BEGIN"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script lacks %q", want)
		}
	}
	if countLinesWithPrefix(script, "BEGIN") != 0 || strings.Contains(script, "COMMIT") {
		t.Fatalf("script opens a transaction:\n%s", script[:400])
	}
	exact := cleanupSQL(7, "test_run_*", names[:500], base, bindNow)
	if strings.Contains(exact, "more schemas follow") ||
		countLinesWithPrefix(exact, "DO ") != 500 {
		t.Fatal("a batch of exactly 500 announces a next batch")
	}
	if one := cleanupSQL(7, "x", names[:1], base, bindNow); len(dropBlocks(t, one)) != 1 {
		t.Fatalf("a one-schema batch:\n%s", one)
	}
}

func TestCleanupSQLKeepsHostileNamesOneLiteral(t *testing.T) {
	names := []string{`a"b;c`, `o'brien`, `back\slash`, `x$sage$y`}
	base := map[string]int64{names[0]: 1, names[1]: 2, names[2]: 3, names[3]: 4}
	script := cleanupSQL(1, "*", names, base, bindNow)
	blocks := dropBlocks(t, script)
	if len(blocks) != 4 {
		t.Fatalf("%d blocks:\n%s", len(blocks), script)
	}
	literals := []string{`'a"b;c'`, `'o''brien'`, `E'back\\slash'`, `'x$sage$y'`}
	const execute = "EXECUTE 'DROP SCHEMA IF EXISTS ' || pg_catalog.quote_ident(v_schema) " +
		"|| ' CASCADE';"
	for i, b := range blocks {
		want := "v_schema text := " + literals[i] + ";"
		if !strings.Contains(b[1], want) {
			t.Fatalf("block %d lacks %q:\n%s", i, want, b[1])
		}
		if strings.Count(b[1], literals[i]) != 1 || !strings.Contains(b[1], execute) {
			t.Fatalf("block %d splices the name outside its literal:\n%s", i, b[1])
		}
		if strings.Contains(b[1], b[0]) {
			t.Fatalf("block %d body contains its own dollar tag %s", i, b[0])
		}
	}
	if blocks[3][0] == "$sage$" {
		t.Fatal("a name holding $sage$ kept the default tag")
	}
	// A pattern is echoed in a "--" comment: a line break in it would
	// turn the rest of the pattern into SQL.
	script = cleanupSQL(1, "t\nDROP DATABASE prod; --\r*", names[:1], base, bindNow)
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(line, "DROP") || strings.Contains(line, "\r") {
			t.Fatalf("the pattern broke out of its comment:\n%s", script)
		}
	}
}

func TestCleanupSQLGuardsEachDropOnActivityAndLocks(t *testing.T) {
	names, base := schemaRange(1)
	blocks := dropBlocks(t, cleanupSQL(3, "test_run_*", names, base, bindNow))
	if len(blocks) != 1 {
		t.Fatalf("%d blocks", len(blocks))
	}
	body := blocks[0][1]
	for _, want := range []string{schemaActivityExpr, "WHERE n.nspname = v_schema",
		"IF v_cur IS DISTINCT FROM v_baseline THEN",
		"RAISE NOTICE 'skip %: activity or stats reset since the batch was built'",
		"pg_catalog.pg_locks", "IS DISTINCT FROM pg_catalog.pg_backend_pid()",
		"EXCEPTION WHEN lock_not_available THEN"} {
		if !strings.Contains(body, want) {
			t.Fatalf("block lacks %q:\n%s", want, body)
		}
	}
	if !strings.Contains(testSchemasSQL, schemaActivityExpr) ||
		!strings.Contains(schemaActivitySQL, schemaActivityExpr) {
		t.Fatal("the batch and the detector read different activity aggregates")
	}
	if strings.Index(body, "pg_locks") > strings.Index(body, "EXECUTE") ||
		strings.Index(body, "v_baseline THEN") > strings.Index(body, "EXECUTE") {
		t.Fatal("a guard runs after the drop")
	}
}

func TestApplyFindingsBatchesOnlySchemasWithABaseline(t *testing.T) {
	facts := []Fact{confirmed(7, TypeTestFixture, KindSchema, "test_memory_*", nil)}
	excluded := []string{"test_memory_aa11bb", "test_memory_cc22dd"}
	activity := map[string]int64{"test_memory_cc22dd": 41}
	out, evaluated := ApplyFindings(facts, nil, excluded, activity, bindNow)
	if len(out) != 1 || !reflect.DeepEqual(evaluated, []string{CategoryTestFixtureCleanup}) {
		t.Fatalf("out %+v evaluated %v", out, evaluated)
	}
	got := out[0].Detail
	if got["schema_count"] != 1 ||
		!reflect.DeepEqual(got["schemas"], []string{"test_memory_cc22dd"}) {
		t.Fatalf("detail %+v", got)
	}
	sql, _ := got["cleanup_sql"].(string)
	if !strings.Contains(sql, "v_baseline bigint := 41;") ||
		strings.Contains(sql, "test_memory_aa11bb") {
		t.Fatalf("batch:\n%s", sql)
	}
	// No baselines at all: no batch, and the category still resolves.
	out, evaluated = ApplyFindings(facts, nil, excluded, nil, bindNow)
	if len(out) != 0 || len(evaluated) != 1 {
		t.Fatalf("without baselines: %+v %v", out, evaluated)
	}
}

type logLine struct{ level, msg string }

func capture(lines *[]logLine) func(string, string, ...any) {
	return func(level, format string, args ...any) {
		*lines = append(*lines, logLine{level, fmt.Sprintf(format, args...)})
	}
}

func TestFindingFilterWithholdsTheBatchWhenActivityCannotBeRead(t *testing.T) {
	var logged []logLine
	src := &fakeConfirmed{facts: []Fact{
		confirmed(7, TypeTestFixture, KindSchema, "test_memory_*", nil)}}
	reader := &fakeActivity{err: errors.New("canceling statement due to statement timeout")}
	f := NewFindingFilter(src, nil, capture(&logged)).WithActivity(reader)
	in := []analyzer.Finding{{Category: "seq_scan_heavy",
		ObjectIdentifier: "test_memory_aa11bb.events"}}
	out, evaluated := f.ApplyFindings(context.Background(), in,
		[]string{"test_memory_aa11bb"})
	if len(out) != 0 || evaluated != nil || reader.calls != 1 {
		t.Fatalf("out %+v evaluated %v calls %d", out, evaluated, reader.calls)
	}
	if len(logged) != 1 || logged[0].level != "WARN" ||
		!strings.Contains(logged[0].msg, "statement timeout") ||
		!strings.Contains(logged[0].msg, "cleanup batch withheld") {
		t.Fatalf("logged %+v", logged)
	}
}

func TestFindingFilterWithoutAReaderOffersNoBatch(t *testing.T) {
	var logged []logLine
	src := &fakeConfirmed{facts: []Fact{
		confirmed(7, TypeTestFixture, KindSchema, "test_memory_*", nil)}}
	f := NewFindingFilter(src, nil, capture(&logged))
	out, evaluated := f.ApplyFindings(context.Background(), nil,
		[]string{"test_memory_aa11bb"})
	if len(out) != 0 || !reflect.DeepEqual(evaluated, []string{CategoryTestFixtureCleanup}) {
		t.Fatalf("out %+v evaluated %v", out, evaluated)
	}
	if len(logged) != 1 || logged[0].level != "WARN" {
		t.Fatalf("logged %+v", logged)
	}
}

func TestFindingFilterReadsActivityOnlyForFixtureSchemas(t *testing.T) {
	src := &fakeConfirmed{facts: []Fact{
		confirmed(7, TypeTestFixture, KindSchema, "test_memory_*", nil)}}
	reader := &fakeActivity{counts: map[string]int64{"test_memory_aa11bb": 0,
		"test_memory_cc22dd": 9}}
	f := NewFindingFilter(src, nil, nil).WithActivity(reader)
	f.now = func() time.Time { return bindNow }
	if out, _ := f.ApplyFindings(context.Background(), nil, nil); len(out) != 0 ||
		reader.calls != 0 {
		t.Fatalf("no fixture schemas: out %+v, %d reads", out, reader.calls)
	}
	excluded := []string{"test_memory_aa11bb", "test_memory_cc22dd"}
	out, _ := f.ApplyFindings(context.Background(), nil, excluded)
	if reader.calls != 1 || !reflect.DeepEqual(reader.asked, excluded) || len(out) != 1 {
		t.Fatalf("calls %d asked %v out %+v", reader.calls, reader.asked, out)
	}
	sql, _ := out[0].Detail["cleanup_sql"].(string)
	if !strings.Contains(sql, "v_baseline bigint := 0;") ||
		!strings.Contains(sql, "v_baseline bigint := 9;") {
		t.Fatalf("baselines not embedded:\n%s", sql)
	}
	if out[0].Detail["generated_at"] != bindNow.UTC().Format(time.RFC3339) {
		t.Fatalf("generated_at %v", out[0].Detail["generated_at"])
	}
}
