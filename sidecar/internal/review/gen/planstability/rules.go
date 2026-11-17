package planstability

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/sqlident"
)

// cliff is one flagged column: the first rule that flagged it, why, and
// every top query that filters on it.
type cliff struct {
	table, column string
	kind, why     string
	queries       []string // evidence ids of the queries
	quantities    map[string]float64
}

// judge applies the rules to the parsed queries.
type judge struct {
	in     inputs
	facts  Facts
	cov    *candidate.Coverage
	cliffs map[string]*cliff // "schema.table.column"
	stx    []candidate.Candidate
}

// unknownTables records each table whose statistics are not readable as
// unknown, with what would make it readable.
func (j *judge) unknownTables(tables []string) {
	for _, t := range tables {
		facts, ok := j.facts.Tables[t]
		why := ""
		switch {
		case !ok:
			why = "the table was not found"
		case facts.Status == StatusNoPrivilege:
			why = fmt.Sprintf("pg_sage's role cannot read its pg_stats rows; run %s",
				grantSQL(t, j.facts.Role))
		case facts.Status == StatusNeverAnalyzed:
			why = "it was never analyzed, so it has no statistics to judge"
		case facts.Status != StatusOK:
			why = "its statistics status is " + facts.Status
		default:
			continue
		}
		j.cov.AddMissing("pg_stats:" + t)
		j.cov.Note("plan stability of %s is unknown, not \"no risk\": %s", t, why)
	}
}

// grantSQL is the GRANT that lets role read table's statistics.
func grantSQL(table, role string) string {
	schema, name, _ := strings.Cut(table, ".")
	if role == "" {
		role = "<pg_sage role>"
	} else {
		role = sqlident.Role(role)
	}
	return "GRANT SELECT ON TABLE " + sqlident.Qualified(schema, name) + " TO " + role
}

// query applies the column rules to each filter, then the correlated rule.
func (j *judge) query(q parsedQuery) {
	eq := map[string][]string{} // table -> equality columns
	for _, p := range q.preds {
		table, col, ok := j.resolve(q, p)
		if !ok {
			continue
		}
		if p.op == opEqual && !containsStr(eq[table], col) {
			eq[table] = append(eq[table], col)
		}
		j.columnRules(q, p, table, col)
	}
	tables := make([]string, 0, len(eq))
	for t := range eq {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, t := range tables {
		if len(eq[t]) >= 2 {
			j.correlated(q, t, eq[t])
		}
	}
}

// resolve is the readable table of q that has the predicate's column: the
// one its qualifier names, or the only one with a column of that name.
func (j *judge) resolve(q parsedQuery, p predicate) (string, string, bool) {
	var found []string
	for _, rel := range q.rels {
		t, ok := j.facts.Tables[rel]
		if !ok || t.Status != StatusOK {
			continue
		}
		if _, has := t.Columns[p.column]; !has {
			continue
		}
		_, name, _ := strings.Cut(rel, ".")
		if p.qualifier != "" && (p.qualifier == name || p.qualifier == rel) {
			return rel, p.column, true
		}
		found = append(found, rel)
	}
	if len(found) != 1 {
		return "", "", false
	}
	return found[0], p.column, true
}

// columnRules flags the column on the first cliff it is on.
func (j *judge) columnRules(q parsedQuery, p predicate, table, column string) {
	t := j.facts.Tables[table]
	c := t.Columns[column]
	if !c.HasStats {
		return
	}
	current, _, _ := j.targets(c)
	kind, why, quantities := cliffOf(p, t, c, current)
	if kind == "" {
		return
	}
	key := table + "." + column
	if found, ok := j.cliffs[key]; ok {
		if !containsStr(found.queries, q.q.ID) {
			found.queries = append(found.queries, q.q.ID)
		}
		return
	}
	j.cliffs[key] = &cliff{table: table, column: column, kind: kind,
		why:     fmt.Sprintf("top query %s filters on it (%s); %s", q.q.QueryID, opText(p.op), why),
		queries: []string{q.q.ID}, quantities: quantities}
}

// cliffOf is the first rule the column is on ("" when none). target is the
// column's effective statistics target: a column with no more distinct
// values than it has every value in its most-common list and no histogram
// for new values to fall past, so the time-ordered rule skips it.
func cliffOf(p predicate, t TableFacts, c ColumnFacts, target int) (string, string,
	map[string]float64) {
	distinct := distinctValues(c.NDistinct, t.RelTuples)
	q := map[string]float64{"null_frac": c.NullFrac, "n_distinct": c.NDistinct}
	if (p.op == opEqual || p.op == opNotNull) && c.NullFrac >= NullFracCliff-1e-12 {
		return KindNullCliff, fmt.Sprintf("it is %.4f%% NULL, so the sample can miss its "+
			"few non-null rows", c.NullFrac*100), q
	}
	if c.TopFreq != nil && *c.TopFreq > SkewTopFreq && distinct > SkewDistinct {
		q["top_freq"] = *c.TopFreq
		return KindSkew, fmt.Sprintf("one value holds %.1f%% of the rows among %.0f "+
			"distinct values, beyond what the sample's other values describe",
			*c.TopFreq*100, distinct), q
	}
	if c.Correlation != nil && *c.Correlation > CorrelationCliff && t.RelTuples > 0 &&
		float64(t.ModSinceAnalyze) > ModShare*t.RelTuples && p.op != opNotNull &&
		distinct > float64(target) {
		q["correlation"] = *c.Correlation
		q["mod_share"] = float64(t.ModSinceAnalyze) / t.RelTuples
		return KindTimeOrdered, fmt.Sprintf("it is inserted in order (correlation %.4f) "+
			"and %.0f%% of the rows changed since the last ANALYZE, so new values lie past "+
			"the histogram", *c.Correlation, q["mod_share"]*100), q
	}
	return "", "", nil
}

// distinctValues is n_distinct as a count (negative: a share of rows).
func distinctValues(nd, reltuples float64) float64 {
	if nd >= 0 {
		return nd
	}
	if reltuples <= 0 {
		return 0
	}
	return -nd * reltuples
}

func opText(op string) string {
	switch op {
	case opNotNull:
		return "IS NOT NULL"
	case opRange:
		return "a range"
	}
	return "="
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
