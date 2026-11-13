package optimizer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sanitize"
	"github.com/pg-sage/sidecar/internal/sqlident"
	"github.com/pg-sage/sidecar/internal/workload"
)

// PostgreSQL 18 btree skip scan (market-15b): an index on (a, b) serves
// WHERE b = $1 when a has few distinct values, so a single-column (b)
// candidate beside it pays its write cost for nothing. Such a candidate is
// dropped only on plan evidence: every top workload statement naming b
// must already scan the composite with an Index Cond on b. Missing
// evidence (no statement, an EXPLAIN error, a plan that does not use it)
// never drops a candidate.

const (
	// skipScanMinMajor is the first major version with btree skip scan.
	skipScanMinMajor = 18
	// skipScanMaxLeadingDistinct is the most distinct values a leading key
	// may have for the rule to consider skip scan.
	skipScanMaxLeadingDistinct = 200
	// skipScanMaxStatements bounds the statements the evidence plans.
	skipScanMaxStatements = 3
	// skipScanReasonPrefix starts the reason of a skip-scan rejection; the
	// serving index's name follows.
	skipScanReasonPrefix = "served_by_skip_scan:"
)

// genericPlanner plans statements for the skip-scan evidence.
type genericPlanner interface {
	genericPlans(ctx context.Context, stmts []string) ([][]byte, error)
}

// skipScanFit is the existing index serving a candidate's column.
type skipScanFit struct {
	index  IndexInfo
	column string
}

// skipScanCandidate reports the existing index that can serve rec through
// skip scan: the server is PostgreSQL 18+, rec is a single plain-column
// btree on c (not unique), and a valid btree on the table has c as its
// second key behind a plain leading key whose known distinct count is at
// most skipScanMaxLeadingDistinct (a negative n_distinct is a fraction of
// the live tuples). The composite must serve rec's predicate and INCLUDE
// columns.
func skipScanCandidate(tc TableContext, rec Recommendation) (IndexInfo, bool) {
	fit, ok := skipScanMatch(tc, rec)
	return fit.index, ok
}

func skipScanMatch(tc TableContext, rec Recommendation) (skipScanFit, bool) {
	if tc.PGMajor < skipScanMinMajor {
		return skipScanFit{}, false
	}
	want, ok := skipShapeOf(rec.DDL)
	if !ok || want.method != "btree" || want.unique || len(want.keys) != 1 ||
		want.keys[0].column == "" {
		return skipScanFit{}, false
	}
	for _, ix := range tc.Indexes {
		if ix.IsValid && servesBySkipScan(tc, ix.Definition, want) {
			return skipScanFit{index: ix, column: want.keys[0].column}, true
		}
	}
	return skipScanFit{}, false
}

// skipShape is an index DDL or definition with its raw keys.
type skipShape struct {
	method, where string
	unique        bool
	keys, include []indexKey
}

func skipShapeOf(ddl string) (skipShape, bool) {
	spec, err := ParseIndexDDL(ddl)
	if err != nil {
		return skipShape{}, false
	}
	keys, include, ok := indexColumns(ddl)
	if !ok {
		return skipShape{}, false
	}
	return skipShape{method: spec.Method, where: canonicalShape(spec.Where),
		unique: spec.Unique, keys: keys, include: include}, true
}

// servesBySkipScan: the definition is a btree whose second key is want's
// key (same collation and operator class) behind a low-cardinality plain
// leading key, and it serves want's predicate and INCLUDE columns.
func servesBySkipScan(tc TableContext, def string, want skipShape) bool {
	have, ok := skipShapeOf(def)
	if !ok || have.method != "btree" || len(have.keys) < 2 || have.keys[0].column == "" ||
		!predicateServes(have.where, want.where, true) {
		return false
	}
	if keyIdentity(have.keys[1], "btree", tc.KeyDefaults) !=
		keyIdentity(want.keys[0], "btree", tc.KeyDefaults) {
		return false
	}
	carried := keyBases(append(slices.Clone(have.keys), have.include...))
	for _, c := range keyBases(want.include) {
		if !slices.Contains(carried, c) {
			return false
		}
	}
	distinct, ok := leadingDistinct(tc, have.keys[0].column)
	return ok && distinct <= skipScanMaxLeadingDistinct
}

// leadingDistinct is col's distinct count from ColStats or
// LeadingKeyStats; ok is false without a known (positive) count.
func leadingDistinct(tc TableContext, col string) (float64, bool) {
	for _, cs := range append(slices.Clone(tc.ColStats), tc.LeadingKeyStats...) {
		if cs.Column != col {
			continue
		}
		d := cs.NDistinct
		if d < 0 {
			d = -d * float64(tc.LiveTuples)
		}
		return d, d > 0
	}
	return 0, false
}

// leadingKeyColumns are the plain leading keys of the table's valid
// multi-key btree indexes that ColStats lacks, on PostgreSQL 18+ only
// (nil before: the context stays as it was).
func leadingKeyColumns(tc TableContext) []string {
	if tc.PGMajor < skipScanMinMajor {
		return nil
	}
	var cols []string
	for _, ix := range tc.Indexes {
		s, ok := skipShapeOf(ix.Definition)
		if !ok || !ix.IsValid || s.method != "btree" || len(s.keys) < 2 ||
			s.keys[0].column == "" {
			continue
		}
		col := s.keys[0].column
		known := slices.ContainsFunc(tc.ColStats, func(cs ColStat) bool {
			return cs.Column == col
		})
		if !known && !slices.Contains(cols, col) {
			cols = append(cols, col)
		}
	}
	return cols
}

// skipScanStatements are the texts of the most-called statements (at most
// skipScanMaxStatements) that name col. ok is false when there is none or
// one of them cannot be planned read-only: the evidence is then
// incomplete.
func skipScanStatements(tc TableContext, col string) ([]string, bool) {
	var named []QueryInfo
	for _, q := range tc.Queries {
		if identIn(q.Text, col) {
			named = append(named, q)
		}
	}
	sort.SliceStable(named, func(i, j int) bool { return named[i].Calls > named[j].Calls })
	named = named[:min(len(named), skipScanMaxStatements)]
	stmts := make([]string, 0, len(named))
	for _, q := range named {
		ok, readOnly := workload.Plannable(q.Text)
		if !ok || !readOnly || sanitize.RejectMultiStatement(q.Text) != nil {
			return nil, false
		}
		stmts = append(stmts, q.Text)
	}
	return stmts, len(stmts) > 0
}

// identIn reports whether text names the column col: its quoted form
// exactly when col needs quoting; otherwise the bare name as a whole
// identifier, case-folded unless it is written quoted (a quoted
// "Created_At" is another column than created_at).
func identIn(text, col string) bool {
	if col == "" {
		return false
	}
	if quoted := sqlident.Ident(col); quoted != col {
		return strings.Contains(text, quoted)
	}
	lower := strings.ToLower(text)
	for i := 0; ; {
		j := strings.Index(lower[i:], col)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(col)
		whole := (start == 0 || !identByte(lower[start-1])) &&
			(end == len(lower) || !identByte(lower[end]))
		quoted := start > 0 && text[start-1] == '"'
		if whole && (!quoted || text[start:end] == col) {
			return true
		}
		i = start + 1
	}
}

func identByte(b byte) bool {
	return b == '_' || b == '$' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b >= 0x80
}

// planNode is the part of an EXPLAIN (FORMAT JSON) node the evidence reads.
type planNode struct {
	IndexName string     `json:"Index Name"`
	IndexCond string     `json:"Index Cond"`
	Plans     []planNode `json:"Plans"`
}

// planStringLiteral is a quoted string constant in a plan condition: it
// is a value, never a column reference.
var planStringLiteral = regexp.MustCompile(`'(?:[^']|'')*'`)

// planServedBy reports whether an EXPLAIN (FORMAT JSON) plan scans index
// with an Index Cond on col anywhere in its tree (string literals in the
// condition are not column references).
func planServedBy(planJSON []byte, index, col string) bool {
	var wrapper []struct {
		Plan planNode `json:"Plan"`
	}
	if json.Unmarshal(planJSON, &wrapper) != nil {
		return false
	}
	var walk func(n planNode) bool
	walk = func(n planNode) bool {
		cond := planStringLiteral.ReplaceAllString(n.IndexCond, "''")
		if n.IndexName == index && identIn(cond, col) {
			return true
		}
		return slices.ContainsFunc(n.Plans, walk)
	}
	for _, w := range wrapper {
		if walk(w.Plan) {
			return true
		}
	}
	return false
}

// servedBySkipScan reports the index already serving rec through skip
// scan, on the evidence of every top statement's generic plan. An
// EXPLAIN failure is logged and is no evidence.
func (o *Optimizer) servedBySkipScan(ctx context.Context, rec Recommendation,
	tc TableContext) (IndexInfo, bool) {
	fit, ok := skipScanMatch(tc, rec)
	if !ok || o.skipScanPlans == nil {
		return IndexInfo{}, false
	}
	stmts, ok := skipScanStatements(tc, fit.column)
	if !ok {
		return IndexInfo{}, false
	}
	plans, err := o.skipScanPlans.genericPlans(ctx, stmts)
	if err == nil && len(plans) != len(stmts) {
		err = fmt.Errorf("%d plans for %d statements", len(plans), len(stmts))
	}
	if err != nil {
		o.logFn("WARN", "optimizer: skip-scan evidence for %s on %s.%s unavailable, the "+
			"candidate is judged as usual: %v", rec.DDL, tc.Schema, tc.Table, err)
		return IndexInfo{}, false
	}
	for _, plan := range plans {
		if !planServedBy(plan, fit.index.Name, fit.column) {
			return IndexInfo{}, false
		}
	}
	return fit.index, true
}

// rejectServedBySkipScan rejects rec as served by ix and remembers the
// rejection, so the next cycle does not plan it again.
func (o *Optimizer) rejectServedBySkipScan(ctx context.Context, rec Recommendation,
	tc TableContext, ix IndexInfo, minPct float64) Admission {
	rec.WhatIf, rec.Validated = WhatIfRejected, false
	rec.WhatIfReason = skipScanReasonPrefix + ix.Name
	rec.EstimatedImprovementPct = 0
	o.memory.remember(ctx, tc, rec, minPct)
	return Admission{Rec: rec, Outcome: AdmitRejected, Reason: rec.WhatIfReason}
}

// remembered is the admission of a candidate rejection memory already
// judged on this workload. A skip-scan rejection holds only while its
// index still serves the candidate: once it is dropped or no longer
// qualifies, the candidate is judged again.
func (o *Optimizer) remembered(ctx context.Context, rec Recommendation, tc TableContext,
	minPct float64) (Admission, bool) {
	r, seen := o.memory.view(ctx, tc).suppress(rec, minPct)
	if !seen {
		return Admission{}, false
	}
	if name, stale := staleSkipScan(r, tc); stale {
		o.logFn("INFO", "optimizer: %s on %s.%s was served by %s through skip scan; "+
			"that no longer holds, judging it again", rec.DDL, tc.Schema, tc.Table, name)
		return Admission{}, false
	}
	o.whatIfSkips.Add(1)
	return Admission{Rec: rec, Outcome: AdmitMeasured, Reason: fmt.Sprintf(
		"already measured %dx, last %s: %s", max(r.MeasureCount, 1),
		r.MeasuredAt.UTC().Format("2006-01-02 15:04 UTC"), r.Reason)}, true
}

// staleSkipScan reports a skip-scan rejection whose index no longer
// serves the remembered candidate on tc (dropped, no longer qualifying, or
// the server version unknown this cycle), with that index's name.
func staleSkipScan(r rejection, tc TableContext) (string, bool) {
	name, ok := strings.CutPrefix(r.Reason, skipScanReasonPrefix)
	if !ok {
		return "", false
	}
	ix, still := skipScanCandidate(tc, Recommendation{DDL: r.DDL})
	return name, !still || ix.Name != name
}

// withoutStaleSkipScan is v without the skip-scan rejections that no
// longer hold on tc, so the agent is not told an idea is served by an
// index that no longer serves it.
func (v *tableMemory) withoutStaleSkipScan(tc TableContext) *tableMemory {
	if v == nil {
		return v
	}
	kept := &tableMemory{mem: v.mem}
	for _, r := range v.live {
		if _, stale := staleSkipScan(r, tc); !stale {
			kept.live = append(kept.live, r)
		}
	}
	return kept
}

// poolPlanner plans the evidence statements on one pooled connection, in
// a read-only transaction under the what-if's bounds (boundSession: the
// statement timeout, the lock timeout and the application's search_path).
// The statements are validated single read-only statements whose $n
// parameters stay unbound (EXPLAIN (GENERIC_PLAN), PostgreSQL 16+; the
// rule runs on 18+ only).
type poolPlanner struct {
	pool        *pgxpool.Pool
	lockTimeout time.Duration
}

func (p poolPlanner) genericPlans(ctx context.Context, stmts []string,
) (plans [][]byte, err error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin the skip-scan evidence transaction: %w", err)
	}
	defer func() {
		rbErr := tx.Rollback(context.WithoutCancel(ctx))
		if rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) && err == nil {
			plans, err = nil, fmt.Errorf("end the skip-scan evidence transaction: %w", rbErr)
		}
	}()
	if err := boundSession(ctx, tx, p.lockTimeout); err != nil {
		return nil, fmt.Errorf("bound the skip-scan evidence transaction: %w", err)
	}
	for _, q := range stmts {
		plan, err := singleJSONPlan(tx.Conn().PgConn().Exec(ctx,
			"EXPLAIN (GENERIC_PLAN, FORMAT JSON) "+q).ReadAll())
		if err != nil {
			return nil, fmt.Errorf("explain a workload statement for skip-scan evidence: %w",
				err)
		}
		plans = append(plans, plan)
	}
	return plans, nil
}
