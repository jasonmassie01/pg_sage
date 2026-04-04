package tuner

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/llm"
)

// Tuner produces per-query tuning findings from plan analysis.
type Tuner struct {
	pool           *pgxpool.Pool
	cfg            TunerConfig
	hintPlan       *HintPlanAvailability
	llmClient      *llm.Client
	fallbackClient *llm.Client
	logFn          func(string, string, ...any)
	recentlyTuned  map[int64]int
}

// Option configures optional Tuner behavior.
type Option func(*Tuner)

// WithLLM enables LLM-enhanced hint reasoning with optional fallback.
func WithLLM(client, fallback *llm.Client) Option {
	return func(t *Tuner) {
		t.llmClient = client
		t.fallbackClient = fallback
	}
}

// New creates a Tuner with the given dependencies.
func New(
	pool *pgxpool.Pool,
	cfg TunerConfig,
	hintPlan *HintPlanAvailability,
	logFn func(string, string, ...any),
	opts ...Option,
) *Tuner {
	t := &Tuner{
		pool:          pool,
		cfg:           cfg,
		hintPlan:      hintPlan,
		logFn:         logFn,
		recentlyTuned: make(map[int64]int),
	}
	for _, o := range opts {
		o(t)
	}
	if t.pool != nil {
		t.loadActiveHints(context.Background())
	}
	return t
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
	t.tickCooldowns()
	candidates, err := t.fetchCandidates(ctx)
	if err != nil {
		return nil, fmt.Errorf("tuner: fetch candidates: %w", err)
	}
	if len(t.recentlyTuned) == 0 {
		t.loadActiveHints(ctx)
	}
	var findings []analyzer.Finding
	for _, c := range candidates {
		if _, ok := t.recentlyTuned[c.QueryID]; ok {
			continue
		}
		f := t.processCandidate(ctx, c)
		if len(f) > 0 {
			t.recentlyTuned[c.QueryID] = t.cooldownCycles()
		}
		findings = append(findings, f...)
	}
	return findings, nil
}

// tickCooldowns decrements all cooldown counters and removes
// entries that have reached zero.
func (t *Tuner) tickCooldowns() {
	for qid, remaining := range t.recentlyTuned {
		remaining--
		if remaining <= 0 {
			delete(t.recentlyTuned, qid)
		} else {
			t.recentlyTuned[qid] = remaining
		}
	}
}

// cooldownCycles returns the configured cascade cooldown,
// defaulting to 3 if unset.
func (t *Tuner) cooldownCycles() int {
	if t.cfg.CascadeCooldownCycles > 0 {
		return t.cfg.CascadeCooldownCycles
	}
	return 3
}

// loadActiveHints bootstraps the recentlyTuned map from
// sage.query_hints so hints survive sidecar restarts.
func (t *Tuner) loadActiveHints(ctx context.Context) {
	if t.pool == nil {
		return
	}
	rows, err := t.pool.Query(ctx,
		`SELECT queryid FROM sage.query_hints
		 WHERE status = 'active'`,
	)
	if err != nil {
		t.logFn("WARN",
			"tuner: load active hints: %v", err)
		return
	}
	defer rows.Close()

	cooldown := t.cooldownCycles()
	for rows.Next() {
		var qid int64
		if err := rows.Scan(&qid); err != nil {
			t.logFn("WARN",
				"tuner: scan active hint: %v", err)
			continue
		}
		t.recentlyTuned[qid] = cooldown
	}
	if err := rows.Err(); err != nil {
		t.logFn("WARN",
			"tuner: iterate active hints: %v", err)
	}
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

	// Always compute deterministic fallback.
	fallback := t.prescribeAll(symptoms)
	fallbackHint := CombineHints(fallback)

	// Try LLM-enhanced reasoning if available.
	prescriptions := t.tryLLMPrescribe(
		ctx, c, symptoms, fallbackHint,
	)
	if len(prescriptions) == 0 {
		prescriptions = fallback
	}
	if len(prescriptions) == 0 {
		return nil
	}

	combined := CombineHints(prescriptions)
	title := buildTitle(symptoms)
	rationale := buildRationale(prescriptions)
	rewrite, rewriteRationale := extractRewrite(prescriptions)
	finding := t.buildFinding(c, symptoms, combined,
		title, rationale, rewrite, rewriteRationale)
	return []analyzer.Finding{finding}
}

func (t *Tuner) tryLLMPrescribe(
	ctx context.Context,
	c candidate,
	symptoms []PlanSymptom,
	fallbackHint string,
) []Prescription {
	if t.llmClient == nil {
		return nil
	}
	planJSON := t.fetchPlanJSON(ctx, c.QueryID)
	if len(symptoms) == 1 && planJSON == "" {
		// Deterministic rules sufficient for single-symptom
		// without plan data; skip LLM to save tokens.
		t.logFn("DEBUG",
			"tuner: single symptom without plan, "+
				"using deterministic rules for queryid=%d",
			c.QueryID)
		return nil
	}
	qctx := buildQueryContext(
		ctx, t.pool, c, symptoms, planJSON, fallbackHint,
	)
	rx, err := llmPrescribe(
		ctx, t.llmClient, t.fallbackClient, qctx, t.logFn,
	)
	if err != nil {
		t.logFn("tuner",
			"LLM prescribe failed for queryid %d, "+
				"using deterministic: %v", c.QueryID, err)
		return nil
	}
	if len(rx) > 0 {
		t.logFn("tuner",
			"LLM-enhanced hints for queryid %d: %s",
			c.QueryID, rx[0].HintDirective)
	}
	return rx
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

	// Stat-based temp spill detection: when no plan-level
	// spill symptoms were found but pg_stat_statements shows
	// temp blocks written, emit a stat-based symptom.
	if c.TempBlksWritten > 0 && !hasSpillSymptom(symptoms) {
		symptoms = append(symptoms, PlanSymptom{
			Kind: SymptomStatTempSpill,
			Detail: map[string]any{
				"temp_blks_written": c.TempBlksWritten,
			},
		})
	}
	return symptoms
}

func (t *Tuner) scanPlanForQuery(
	ctx context.Context, queryID int64,
) []PlanSymptom {
	planJSON := t.fetchPlanJSON(ctx, queryID)
	if planJSON == "" {
		return nil
	}
	symptoms, err := ScanPlan([]byte(planJSON))
	if err != nil {
		t.logFn("tuner", "scan plan for queryid %d: %v",
			queryID, err)
		return nil
	}
	return symptoms
}

func (t *Tuner) fetchPlanJSON(
	ctx context.Context, queryID int64,
) string {
	var planJSON []byte
	err := t.pool.QueryRow(ctx,
		`SELECT plan_json FROM sage.explain_cache
		 WHERE queryid = $1
		 ORDER BY captured_at DESC LIMIT 1`,
		queryID,
	).Scan(&planJSON)
	if err != nil {
		return ""
	}
	return string(planJSON)
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
	suggestedRewrite, rewriteRationale string,
) analyzer.Finding {
	names := symptomNames(symptoms)
	detail := map[string]any{
		"queryid":        c.QueryID,
		"query":          c.Query,
		"symptoms":       names,
		"hint_directive": combinedHint,
	}
	if suggestedRewrite != "" {
		detail["suggested_rewrite"] = suggestedRewrite
		detail["rewrite_rationale"] = rewriteRationale
	}
	f := analyzer.Finding{
		Category:         "query_tuning",
		Severity:         "warning",
		ObjectType:       "query",
		ObjectIdentifier: fmt.Sprintf("queryid:%d", c.QueryID),
		Title:            title,
		Detail:           detail,
		Recommendation:   rationale,
		ActionRisk:       "safe",
	}
	if t.hintPlan != nil && t.hintPlan.Available && t.hintPlan.HintTableReady {
		f.RecommendedSQL = BuildInsertSQL(
			c.QueryID, combinedHint,
		)
		f.RollbackSQL = BuildDeleteSQL(c.QueryID)
	}

	// Persist to sage.query_hints for the dashboard query-hints page.
	t.upsertQueryHint(c.QueryID, combinedHint,
		strings.Join(names, ", "), suggestedRewrite, rewriteRationale)

	return f
}

// upsertQueryHint writes a record to sage.query_hints so the
// dashboard query-hints page displays tuner findings.
func (t *Tuner) upsertQueryHint(
	queryID int64, hintText, symptom,
	suggestedRewrite, rewriteRationale string,
) {
	if t.pool == nil {
		return
	}
	// Update existing active hint, or insert new one.
	tag, err := t.pool.Exec(context.Background(),
		`UPDATE sage.query_hints
		 SET hint_text = $2, symptom = $3,
		     suggested_rewrite = $4, rewrite_rationale = $5
		 WHERE queryid = $1 AND status = 'active'`,
		queryID, hintText, symptom,
		suggestedRewrite, rewriteRationale,
	)
	if err != nil {
		t.logFn("WARN", "tuner: update query_hint: %v", err)
		return
	}
	if tag.RowsAffected() == 0 {
		_, err = t.pool.Exec(context.Background(),
			`INSERT INTO sage.query_hints
				(queryid, hint_text, symptom,
				 suggested_rewrite, rewrite_rationale, status)
			 VALUES ($1, $2, $3, $4, $5, 'active')`,
			queryID, hintText, symptom,
			suggestedRewrite, rewriteRationale,
		)
		if err != nil {
			t.logFn("WARN", "tuner: insert query_hint: %v", err)
		}
	}
}

// BuildInsertSQL generates an INSERT for hint_plan.hints.
func BuildInsertSQL(queryID int64, hint string) string {
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

// BuildDeleteSQL generates a DELETE for hint_plan.hints.
func BuildDeleteSQL(queryID int64) string {
	return fmt.Sprintf(
		"DELETE FROM hint_plan.hints "+
			"WHERE query_id = %d "+
			"AND application_name = ''",
		queryID,
	)
}

// hasSpillSymptom returns true if any symptom indicates a
// disk sort or hash spill (plan-level detection).
func hasSpillSymptom(symptoms []PlanSymptom) bool {
	for _, s := range symptoms {
		switch s.Kind {
		case SymptomDiskSort, SymptomHashSpill:
			return true
		}
	}
	return false
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
	case SymptomStatTempSpill:
		return "Per-query tuning: increase work_mem " +
			"for temp-spilling query"
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
	if kinds[SymptomStatTempSpill] {
		parts = append(parts, "work_mem")
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

// extractRewrite returns the first non-empty suggested rewrite
// from the prescription list.
func extractRewrite(
	prescriptions []Prescription,
) (rewrite, rationale string) {
	for _, p := range prescriptions {
		if p.SuggestedRewrite != "" {
			return p.SuggestedRewrite, p.RewriteRationale
		}
	}
	return "", ""
}
