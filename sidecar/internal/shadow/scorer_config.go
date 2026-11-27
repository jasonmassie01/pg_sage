package shadow

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/extstats"
	"github.com/pg-sage/sidecar/internal/verify"
)

// reloptionExternal: the autovacuum reloptions p proposes, set on its
// table outside pg_sage to exactly the proposed values. A decision whose
// SQL is no parsable ALTER TABLE ... SET (key = value, ...) has no
// detector; it is unscored at the horizon.
func (s *Scorer) reloptionExternal(ctx context.Context, p pendingDecision) (*verifiedFact,
	bool, error) {
	table, want, ok := parseReloptions(p.SQL)
	if !ok {
		return nil, false, nil
	}
	marked := p.AppliedDetectedAt != nil && p.AppliedAfter != nil
	if !marked {
		resolved, err := s.appName(ctx, table)
		if err != nil {
			return catalogFailure("resolve "+table, err)
		}
		var gone bool
		var have []string
		err = s.catalog.QueryRow(ctx, `/* pg_sage */ SELECT r.oid IS NULL,
			COALESCE(c.reloptions, '{}')::text[]
			FROM (SELECT to_regclass($1) AS oid) r
			LEFT JOIN pg_class c ON c.oid = r.oid`, resolved).Scan(&gone, &have)
		switch {
		case err != nil:
			return catalogFailure("read the reloptions of "+table, err)
		case gone:
			return relationGone(table), false, nil
		case !reloptionsMatch(have, want):
			return nil, false, nil
		}
	}
	return s.verifyExternal(ctx, p, table, marked)
}

// parseReloptions reads ALTER TABLE [IF EXISTS] table SET (key = value,
// ...): the table as written and each option as pg_class.reloptions holds
// it (key=value, the key lower-cased, a quoted value unquoted).
func parseReloptions(sql string) (string, []string, bool) {
	toks := tokenize(sql)
	for len(toks) > 0 && toks[len(toks)-1].text == ";" {
		toks = toks[:len(toks)-1]
	}
	if !words(toks, 0, "alter", "table") {
		return "", nil, false
	}
	rest := toks[2:]
	if words(rest, 0, "if", "exists") {
		rest = rest[2:]
	}
	set := slices.IndexFunc(rest, func(t token) bool { return !t.punct && t.text == "set" })
	if set < 1 || set+2 >= len(rest) || rest[set+1].text != "(" ||
		rest[len(rest)-1].text != ")" {
		return "", nil, false
	}
	var opts []string
	for _, group := range splitTokens(rest[set+2:len(rest)-1], ",") {
		opt, ok := reloption(group)
		if !ok {
			return "", nil, false
		}
		opts = append(opts, opt)
	}
	return join(rest[:set]), opts, true
}

// reloption is one key = value of a SET list.
func reloption(group []token) (string, bool) {
	if len(group) < 3 || group[0].punct || group[1].text != "=" {
		return "", false
	}
	for _, t := range group[2:] {
		if t.text == "(" || t.text == ")" || t.text == "=" {
			return "", false
		}
	}
	value := join(group[2:])
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		value = strings.ReplaceAll(value[1:len(value)-1], "''", "'")
	}
	return group[0].text + "=" + value, value != ""
}

// splitTokens splits toks at each sep punctuation token.
func splitTokens(toks []token, sep string) [][]token {
	var out [][]token
	start := 0
	for i, t := range toks {
		if t.punct && t.text == sep {
			out = append(out, toks[start:i])
			start = i + 1
		}
	}
	return append(out, toks[start:])
}

// reloptionsMatch: every proposed option is in the table's reloptions,
// value for value.
func reloptionsMatch(have, want []string) bool {
	if len(want) == 0 {
		return false
	}
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// statisticsSQL lists a table's extended statistics objects: their kinds
// (pg_statistic_ext.stxkind), plain columns and whether they cover
// expressions (stxexprs, PG14+). A table without any yields one row of
// NULLs; a gone table one row with no oid.
const statisticsSQL = `/* pg_sage */ SELECT r.oid IS NULL,
	COALESCE(s.stxkind::text[], '{}'),
	COALESCE(ARRAY(SELECT a.attname::text FROM unnest(s.stxkeys::int2[]) k(n)
	               JOIN pg_attribute a ON a.attrelid = s.stxrelid AND a.attnum = k.n),
	         '{}'),
	COALESCE(s.stxexprs IS NOT NULL, false)
	FROM (SELECT to_regclass($1) AS oid) r
	LEFT JOIN pg_statistic_ext s ON s.stxrelid = r.oid`

// statisticsExternal: an extended statistics object of the same columns
// and kinds as p's, created on its table outside pg_sage (any name).
func (s *Scorer) statisticsExternal(ctx context.Context, p pendingDecision) (*verifiedFact,
	bool, error) {
	c, err := extstats.ParseCreate(p.SQL)
	if err != nil {
		return nil, false, nil // no pg_sage statistics form: nothing to compare
	}
	table := c.QualifiedTable()
	marked := p.AppliedDetectedAt != nil && p.AppliedAfter != nil
	if !marked {
		found, gone, err := s.statisticsFound(ctx, c, table)
		switch {
		case err != nil:
			return catalogFailure("read the extended statistics of "+table, err)
		case gone:
			return relationGone(table), false, nil
		case !found:
			return nil, false, nil
		}
	}
	return s.verifyExternal(ctx, p, table, marked)
}

func (s *Scorer) statisticsFound(ctx context.Context, c extstats.Create,
	table string) (found, gone bool, err error) {
	rows, err := s.catalog.Query(ctx, statisticsSQL, table)
	if err != nil {
		return false, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var kinds, columns []string
		var exprs bool
		if err := rows.Scan(&gone, &kinds, &columns, &exprs); err != nil {
			return false, false, err
		}
		if !gone && statisticsMatch(c, kinds, columns, exprs) {
			found = true
		}
	}
	return found, gone, rows.Err()
}

// statisticsKind is the stxkind letter of each kind pg_sage proposes.
var statisticsKind = map[string]string{"ndistinct": "d", "dependencies": "f", "mcv": "m"}

// statisticsMatch: an object of exactly c's kinds (all three when c names
// none) on exactly c's columns, without expressions. PostgreSQL keeps the
// columns in attribute order, so they are compared as a set.
func statisticsMatch(c extstats.Create, kinds, columns []string, exprs bool) bool {
	want := []string{"d", "f", "m"}
	if len(c.Kinds) > 0 {
		want = want[:0]
		for _, k := range c.Kinds {
			want = append(want, statisticsKind[k])
		}
	}
	return !exprs && sameSet(kinds, want) && sameSet(columns, c.Columns)
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// decideStatistics completes f, the targets' time comparison of an
// extended statistics object, with their row estimates over the same
// windows (verify.DecideStatistics).
func (s *Scorer) decideStatistics(ctx context.Context, p pendingDecision, f *verifiedFact,
	latency *verify.Comparison, win time.Duration) error {
	source := verify.NewPostgresObservationSource(s.pool)
	ids := p.Prediction.TargetQueryIDs
	before, err := source.EstimateErrors(ctx, ids, p.AppliedAfter.Add(-win), *p.AppliedAfter)
	if err != nil {
		return fmt.Errorf("shadow: estimates before decision %d: %w", p.ID, err)
	}
	after, err := source.EstimateErrors(ctx, ids, *p.AppliedDetectedAt,
		p.AppliedDetectedAt.Add(win))
	if err != nil {
		return fmt.Errorf("shadow: estimates after decision %d: %w", p.ID, err)
	}
	est := verify.JudgeEstimates(before, after, 0)
	f.Verdict, f.Reason, _ = verify.DecideStatistics(latency, est)
	f.Evidence["verdict"] = f.Verdict
	f.Evidence["estimates"] = map[string]any{"verdict": est.Verdict, "reason": est.Reason,
		"before": before, "after": after}
	return nil
}
