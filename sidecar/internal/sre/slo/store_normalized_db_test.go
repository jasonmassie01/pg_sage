package slo

import (
	"context"
	"fmt"
	"regexp"
	"testing"
)

// The performance gate explains pg_stat_statements' normalized texts,
// whose constants are untyped parameters: a CASE whose branches are all
// constants (THEN 1 ELSE 0) then resolves to text, and "bigint + text" or
// "sum(text)" left the running-counter writes unexplainable (gate
// "Unexplainable statements"), so no plan check covered them. Every SLO
// statement must still plan with its constants as untyped parameters.

var (
	normalizedNumber = regexp.MustCompile(`(^|[^$\w.])(\d+(\.\d+)?)\b`)
	normalizedString = regexp.MustCompile(`'[^']*'`)
	paramRef         = regexp.MustCompile(`\$(\d+)`)
)

// normalizeConstants replaces a statement's literals with fresh
// parameters, as pg_stat_statements does.
func normalizeConstants(sql string) string {
	next := 0
	for _, m := range paramRef.FindAllStringSubmatch(sql, -1) {
		var n int
		_, _ = fmt.Sscan(m[1], &n)
		next = max(next, n)
	}
	fresh := func() string { next++; return fmt.Sprintf("$%d", next) }
	sql = normalizedString.ReplaceAllStringFunc(sql, func(string) string { return fresh() })
	return normalizedNumber.ReplaceAllStringFunc(sql, func(m string) string {
		sub := normalizedNumber.FindStringSubmatch(m)
		return sub[1] + fresh()
	})
}

func TestNormalizeConstants(t *testing.T) {
	got := normalizeConstants("SELECT $2, x1, 1.5, 'a', ($1 - 10)")
	want := "SELECT $2, x1, $4, $3, ($1 - $5)"
	if got != want {
		t.Fatalf("normalizeConstants = %q, want %q", got, want)
	}
}

func TestSLOStatements_PlanWithNormalizedConstants(t *testing.T) {
	pool, ctx := livePool(t)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	defer func() { _, _ = conn.Exec(context.Background(), "DEALLOCATE ALL") }()
	stmts := map[string]string{"insert": insertSampleSQL, "rechain": rechainSQL,
		"one series": oneSeriesPointsSQL, "all series": allSeriesPointsSQL,
		"raw series": rawSeriesSQL}
	for name, sql := range stmts {
		norm := normalizeConstants(sql)
		if norm == sql {
			t.Fatalf("%s: no constant was normalized; the check would prove nothing", name)
		}
		_, err := conn.Exec(ctx, "PREPARE slo_norm AS "+norm)
		if err != nil {
			t.Errorf("%s does not plan once normalized: %v\n%s", name, err, norm)
			continue
		}
		if _, err := conn.Exec(ctx, "DEALLOCATE slo_norm"); err != nil {
			t.Fatal(err)
		}
	}
}
