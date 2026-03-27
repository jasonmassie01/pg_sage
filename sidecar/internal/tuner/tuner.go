package tuner

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
)

// Tuner produces per-query tuning findings from plan analysis.
type Tuner struct {
	pool     *pgxpool.Pool
	cfg      TunerConfig
	hintPlan *HintPlanAvailability
	logFn    func(string, string, ...any)
}

// New creates a Tuner with the given dependencies.
func New(
	pool *pgxpool.Pool,
	cfg TunerConfig,
	hintPlan *HintPlanAvailability,
	logFn func(string, string, ...any),
) *Tuner {
	return &Tuner{
		pool:     pool,
		cfg:      cfg,
		hintPlan: hintPlan,
		logFn:    logFn,
	}
}

type candidate struct {
	QueryID          int64
	Query            string
	Calls            int64
	MeanExecTime     float64
	MeanPlanTime     float64
	TempBlksRead     int64
	TempBlksWritten  int64
}

// Tune queries pg_stat_statements for slow queries, scans
// their plans, and returns actionable findings.
func (t *Tuner) Tune(
	ctx context.Context,
) ([]analyzer.Finding, error) {
	candidates, err := t.fetchCandidates(ctx)
	if err != nil {
		return nil, fmt.Errorf("tuner: fetch candidates: %w", err)
	}
	var findings []analyzer.Finding
	for _, c := range candidates {
		f := t.processCandidate(ctx, c)
		findings = append(findings, f...)
	}
	return findings, nil
}

const candidateSQL = `
SELECT queryid, query, calls, mean_exec_time,
       mean_plan_time, temp_blks_read, temp_blks_written
FROM pg_stat_statements
WHERE calls >= $1
  AND (mean_exec_time > 100
       OR temp_blks_written > 0
       OR (mean_plan_time > 0
           AND mean_plan_time > mean_exec_time * $2))
ORDER BY mean_exec_time * calls DESC
LIMIT 50`

func (t *Tuner) fetchCandidates(
	ctx context.Context,
) ([]candidate, error) {
	rows, err := t.pool.Query(
		ctx, candidateSQL,
		t.cfg.MinQueryCalls, t.cfg.PlanTimeRatio,
	)
	if err != nil {
		return nil, fmt.Errorf("query pg_stat_statements: %w", err)
	}
	defer rows.Close()

	var out []candidate
	for rows.Next() {
		var c candidate
		err := rows.Scan(
			&c.QueryID, &c.Query, &c.Calls,
			&c.MeanExecTime, &c.MeanPlanTime,
			&c.TempBlksRead, &c.TempBlksWritten,
		)
		if err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (t *Tuner) processCandidate(
	ctx context.Context, c candidate,
) []analyzer.Finding {
	symptoms := t.gatherSymptoms(ctx, c)
	if len(symptoms) == 0 {
		return nil
	}
	prescriptions := t.prescribeAll(symptoms)
	if len(prescriptions) == 0 {
		return nil
	}
	combined := CombineHints(prescriptions)
	title := buildTitle(symptoms)
	rationale := buildRationale(prescriptions)
	finding := t.buildFinding(c, symptoms, combined,
		title, rationale)
	return []analyzer.Finding{finding}
}

func (t *Tuner) gatherSymptoms(
	ctx context.Context, c candidate,
) []PlanSymptom {
	var symptoms []PlanSymptom
	planSymptoms := t.scanPlanForQuery(ctx, c.QueryID)
	symptoms = append(symptoms, planSymptoms...)

	if t.isHighPlanTime(c) {
		symptoms = append(symptoms, PlanSymptom{
			Kind: SymptomHighPlanTime,
		})
	}
	return symptoms
}

func (t *Tuner) scanPlanForQuery(
	ctx context.Context, queryID int64,
) []PlanSymptom {
	var planJSON []byte
	err := t.pool.QueryRow(ctx,
		`SELECT plan_json FROM sage.explain_cache
		 WHERE queryid = $1
		 ORDER BY captured_at DESC LIMIT 1`,
		queryID,
	).Scan(&planJSON)
	if err != nil {
		return nil
	}
	symptoms, err := ScanPlan(planJSON)
	if err != nil {
		t.logFn("WARN", "tuner: scan plan for queryid %d: %v",
			queryID, err)
		return nil
	}
	return symptoms
}

func (t *Tuner) isHighPlanTime(c candidate) bool {
	return c.MeanPlanTime > 0 &&
		c.MeanPlanTime > c.MeanExecTime*t.cfg.PlanTimeRatio &&
		c.Calls >= int64(t.cfg.MinQueryCalls)
}

func (t *Tuner) prescribeAll(
	symptoms []PlanSymptom,
) []Prescription {
	var out []Prescription
	for _, s := range symptoms {
		p := Prescribe(s, t.cfg)
		if p != nil {
			out = append(out, *p)
		}
	}
	return out
}

func (t *Tuner) buildFinding(
	c candidate,
	symptoms []PlanSymptom,
	combinedHint, title, rationale string,
) analyzer.Finding {
	names := symptomNames(symptoms)
	f := analyzer.Finding{
		Category:         "query_tuning",
		Severity:         "warning",
		ObjectType:       "query",
		ObjectIdentifier: fmt.Sprintf("queryid:%d", c.QueryID),
		Title:            title,
		Detail: map[string]any{
			"queryid":        c.QueryID,
			"query":          c.Query,
			"symptoms":       names,
			"hint_directive": combinedHint,
		},
		Recommendation: rationale,
		ActionRisk:     "safe",
	}
	if t.hintPlan.Available && t.hintPlan.HintTableReady {
		f.RecommendedSQL = buildInsertSQL(
			c.QueryID, combinedHint,
		)
		f.RollbackSQL = buildDeleteSQL(c.QueryID)
	}
	return f
}

func buildInsertSQL(queryID int64, hint string) string {
	escapedHint := strings.ReplaceAll(hint, "'", "''")
	return fmt.Sprintf(
		"INSERT INTO hint_plan.hints "+
			"(query_id, application_name, hints) "+
			"VALUES (%d, '', '%s') "+
			"ON CONFLICT (query_id, application_name) "+
			"DO UPDATE SET hints = EXCLUDED.hints",
		queryID, escapedHint,
	)
}

func buildDeleteSQL(queryID int64) string {
	return fmt.Sprintf(
		"DELETE FROM hint_plan.hints "+
			"WHERE query_id = %d "+
			"AND application_name = ''",
		queryID,
	)
}

func symptomNames(symptoms []PlanSymptom) []string {
	out := make([]string, len(symptoms))
	for i, s := range symptoms {
		out[i] = string(s.Kind)
	}
	return out
}

func buildTitle(symptoms []PlanSymptom) string {
	if len(symptoms) == 1 {
		return singleSymptomTitle(symptoms[0].Kind)
	}
	return multiSymptomTitle(symptoms)
}

func singleSymptomTitle(kind SymptomKind) string {
	switch kind {
	case SymptomDiskSort:
		return "Per-query tuning: increase work_mem " +
			"for disk-sorting query"
	case SymptomHashSpill:
		return "Per-query tuning: increase work_mem " +
			"for hash-spilling query"
	case SymptomHighPlanTime:
		return "Per-query tuning: force generic plan " +
			"for high-planning-time query"
	case SymptomBadNestedLoop:
		return "Per-query tuning: fix nested loop " +
			"join strategy"
	case SymptomSeqScanWithIndex:
		return "Per-query tuning: force index scan " +
			"for seq-scanning query"
	case SymptomParallelDisabled:
		return "Per-query tuning: enable parallel " +
			"workers for scan"
	default:
		return "Per-query tuning recommendation"
	}
}

func multiSymptomTitle(symptoms []PlanSymptom) string {
	kinds := make(map[SymptomKind]bool)
	for _, s := range symptoms {
		kinds[s.Kind] = true
	}
	var parts []string
	if kinds[SymptomDiskSort] || kinds[SymptomHashSpill] {
		parts = append(parts, "work_mem")
	}
	if kinds[SymptomHighPlanTime] {
		parts = append(parts, "generic plan")
	}
	if kinds[SymptomBadNestedLoop] {
		parts = append(parts, "join strategy")
	}
	if kinds[SymptomSeqScanWithIndex] {
		parts = append(parts, "index scan")
	}
	if kinds[SymptomParallelDisabled] {
		parts = append(parts, "parallel workers")
	}
	if len(parts) == 0 {
		return "Per-query tuning recommendation"
	}
	return fmt.Sprintf(
		"Per-query tuning: %s for query",
		strings.Join(parts, " + "),
	)
}

func buildRationale(prescriptions []Prescription) string {
	parts := make([]string, len(prescriptions))
	for i, p := range prescriptions {
		parts[i] = p.Rationale
	}
	return strings.Join(parts, "; ")
}
