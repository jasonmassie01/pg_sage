package tuning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/extstats"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/tuner"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Extended statistics in the accepted form (PR #110) and per-statement
// hints through the tuner.

var (
	plainColumn = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)
	nameUnsafe  = regexp.MustCompile(`[^a-z0-9_]+`)
	statsKinds  = []string{"ndistinct", "dependencies", "mcv"}
)

// judgeStatistics admits a CREATE STATISTICS on 2-8 plain columns of a
// case table, named by pg_sage, in the one form the executor runs.
func (v *validator) judgeStatistics(ctx context.Context, c Case, p Proposal) Judged {
	table, j, ok := v.tableInCase(c, p, p.Table)
	if !ok {
		return j
	}
	cols, kinds, why := statsShape(p)
	if why != "" {
		return reject(p, ReasonUnsupported, "%s", why)
	}
	if _, why := predictedChange(p, true); why != "" {
		return reject(p, ReasonInvalid, "%s", why)
	}
	schema, rel, _ := splitQualified(table)
	if j, ok := v.statsColumnsExist(ctx, p, table, cols); !ok {
		return j
	}
	existing, err := v.a.deps.Store.ExtendedStats(ctx, schema, rel)
	if err != nil {
		return reject(p, ReasonUnavailable, "extended statistics unreadable: %v", err)
	}
	for _, e := range existing {
		if sameSet(e.Columns, cols) {
			return reject(p, ReasonDuplicate, "%s already covers (%s)", e.Name,
				strings.Join(cols, ", "))
		}
	}
	f, err := statsFinding(table, schema, rel, cols, kinds, p)
	if err != nil {
		return reject(p, ReasonUnsupported, "%v", err)
	}
	pred := verify.Prediction{Class: verify.ClassStatistics, Method: verify.MethodModel,
		Metric: verify.MetricRowEstimateError, ExpectedChangePct: p.ExpectedChangePct,
		TargetQueryIDs: v.targets(c, p), Source: PredictionSource,
		Note: "the model's estimate of the row-estimate error"}
	return Judged{Proposal: p, Verdict: VerdictAdmitted, Finding: &f, Tables: []string{table},
		Class: verify.ClassStatistics, Prediction: pred}
}

// statsShape checks the columns and kinds and returns them canonical:
// columns sorted, kinds in PostgreSQL's order (all three when none given).
func statsShape(p Proposal) ([]string, []string, string) {
	if len(p.Columns) < 2 || len(p.Columns) > 8 {
		return nil, nil, fmt.Sprintf("extended statistics take 2 to 8 columns, got %d",
			len(p.Columns))
	}
	cols := make([]string, 0, len(p.Columns))
	for _, col := range p.Columns {
		col = strings.TrimSpace(col)
		if !plainColumn.MatchString(col) {
			return nil, nil, fmt.Sprintf("%q is not a plain column (no expressions)", col)
		}
		if slices.Contains(cols, col) {
			return nil, nil, fmt.Sprintf("column %s is repeated", col)
		}
		cols = append(cols, col)
	}
	slices.Sort(cols)
	kinds := statsKinds
	if len(p.Kinds) > 0 {
		kinds = nil
		for _, k := range statsKinds {
			if slices.Contains(p.Kinds, k) {
				kinds = append(kinds, k)
			}
		}
		for _, k := range p.Kinds {
			if !slices.Contains(statsKinds, k) {
				return nil, nil, fmt.Sprintf("statistics kind %q is not supported", k)
			}
		}
	}
	return cols, kinds, ""
}

func (v *validator) statsColumnsExist(ctx context.Context, p Proposal, table string,
	cols []string) (Judged, bool) {
	tc, ok := v.a.tableContext(ctx, v.cur, table)
	if !ok || len(tc.Columns) == 0 {
		return reject(p, ReasonUnavailable, "the columns of %s are unknown", table), false
	}
	for _, col := range cols {
		if !slices.ContainsFunc(tc.Columns, func(ci optimizer.ColumnInfo) bool {
			return ci.Name == col
		}) {
			return reject(p, ReasonInvalid, "%s has no column %s", table, col), false
		}
	}
	return Judged{}, true
}

func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// statsName is pg_sage's name for the statistics on cols of rel: the
// prefix, the table and a hash of the sorted column set, within 63 bytes.
func statsName(rel string, cols []string) string {
	sum := sha256.Sum256([]byte(strings.Join(cols, ",")))
	suffix := "_" + hex.EncodeToString(sum[:])[:8]
	base := nameUnsafe.ReplaceAllString(strings.ToLower(rel), "_")
	if room := 63 - len(extstats.NamePrefix) - len(suffix); len(base) > room {
		base = base[:room]
	}
	return extstats.NamePrefix + base + suffix
}

func statsFinding(table, schema, rel string, cols, kinds []string,
	p Proposal) (analyzer.Finding, error) {
	name := statsName(rel, cols)
	quoted := make([]string, len(cols))
	for i, col := range cols {
		quoted[i] = quoteIdent(col)
	}
	sql := fmt.Sprintf("CREATE STATISTICS IF NOT EXISTS %s.%s (%s) ON %s FROM %s",
		quoteIdent(schema), name, strings.Join(kinds, ", "), strings.Join(quoted, ", "), table)
	parsed, err := extstats.ParseCreate(sql)
	if err != nil {
		return analyzer.Finding{}, fmt.Errorf("not the accepted statistics form: %w", err)
	}
	return analyzer.Finding{Category: CategoryStatistics, Severity: "info",
		ObjectType: "table", ObjectIdentifier: table + ":" + name,
		Title: fmt.Sprintf("Extended statistics on %s (%s)", table, strings.Join(cols, ", ")),
		Detail: map[string]any{"table": table, "statistics": name, "columns": cols,
			"kinds": kinds, "action_risk": "moderate"},
		Recommendation: p.Rationale, RecommendedSQL: sql, RollbackSQL: parsed.Rollback(),
		ActionRisk: "moderate"}, nil
}

// judgeHint admits a pg_hint_plan hint for a case statement through the
// tuner, which validates, clamps and records it.
func (v *validator) judgeHint(ctx context.Context, c Case, p Proposal) Judged {
	qid := int64(p.QueryID)
	idx := slices.IndexFunc(c.Statements, func(s CaseStatement) bool {
		return s.QueryID == qid
	})
	if qid == 0 || idx < 0 {
		return reject(p, ReasonOutOfCase, "queryid %d is not a statement of case %s", qid,
			c.ID)
	}
	if _, why := predictedChange(p, true); why != "" {
		return reject(p, ReasonInvalid, "%s", why)
	}
	f, err := v.a.deps.Hints.ProposeHint(ctx, tuner.HintProposal{QueryID: qid,
		Query: c.Statements[idx].Text, Hint: p.Hint, Rationale: p.Rationale,
		Detail: map[string]any{"producer": Producer, "case_id": c.ID}})
	switch {
	case errors.Is(err, tuner.ErrHintsUnavailable):
		return reject(p, ReasonDisabled, "%v", err)
	case errors.Is(err, tuner.ErrHintExists):
		return reject(p, ReasonDuplicate, "%v", err)
	case err != nil:
		return reject(p, ReasonInvalid, "the tuner refused the hint: %v", err)
	}
	pred := verify.Prediction{Class: verify.ClassQueryHint, Method: verify.MethodModel,
		Metric: verify.MetricMeanExecTime, ExpectedChangePct: p.ExpectedChangePct,
		TargetQueryIDs: []int64{qid}, Source: PredictionSource,
		Note: "the model's estimate"}
	return Judged{Proposal: p, Verdict: VerdictAdmitted, Finding: &f, Tables: c.Tables,
		Class: verify.ClassQueryHint, Prediction: pred}
}
