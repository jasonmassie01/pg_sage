package tuner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

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

	// mu serializes Tune() and Revalidate() so the two loops
	// never interleave writes to sage.query_hints.
	mu sync.Mutex

	// staleStats is the per-cycle cache of pg_stat_user_tables
	// information; reloaded at the start of every Tune() call.
	staleStats *StaleStatsCache

	// staleStatsEmitted tracks tables already emitted as
	// stale_statistics findings during the current cycle so
	// multiple queries touching the same stale table produce
	// exactly one ANALYZE finding.
	staleStatsEmitted map[string]bool

	llmPrescriptionCooldown map[string]int
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
		pool:                    pool,
		cfg:                     cfg,
		hintPlan:                hintPlan,
		logFn:                   logFn,
		recentlyTuned:           make(map[int64]int),
		llmPrescriptionCooldown: make(map[string]int),
	}
	for _, o := range opts {
		o(t)
	}
	// Active hints are loaded on the first Tune() call when
	// recentlyTuned is empty (see Tune()), so bootstrap here is
	// unnecessary and would require a context we don't have.
	return t
}

type candidate struct {
	QueryID         int64
	Query           string
	Calls           int64
	MeanExecTime    float64
	MeanPlanTime    float64
	TempBlksRead    int64
	TempBlksWritten int64
}

// Tune queries pg_stat_statements for slow queries, scans
// their plans, and returns actionable findings.
//
// deferredTables names "schema.table" entries that have pending
// index optimizer recommendations. The tuner skips any candidate
// whose plan reads from one of these tables — applying a hint
// (e.g. HashJoin) just before a covering index lands risks
// installing a directive that becomes obsolete or actively wrong
// once the index is created. The deferred set must be canonical
// "schema.table" form (use CanonicalizeTableRef on inputs).
// nil/empty disables gating, preserving prior behaviour.
func (t *Tuner) Tune(
	ctx context.Context,
	deferredTables map[string]bool,
) ([]analyzer.Finding, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Reload stale-stats cache and dedup set for this cycle.
	cache, staleErr := LoadStaleStatsCache(ctx, t.pool, t.cfg)
	if staleErr != nil {
		t.logFn("WARN",
			"tuner: load stale stats cache: %v", staleErr)
	}
	t.staleStats = cache
	t.staleStatsEmitted = map[string]bool{}

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
		if reason := t.deferralReason(ctx, c, deferredTables); reason != "" {
			t.logFn("INFO",
				"tuner: deferring queryid=%d, %s "+
					"(optimizer recommendation pending)",
				c.QueryID, reason)
			continue
		}
		f := t.processCandidate(ctx, c)
		if len(f) > 0 {
			t.recordTuned(c.QueryID)
		}
		findings = append(findings, f...)
	}
	return findings, nil
}

// maxRecentlyTuned caps the cooldown map so that a pathological
// workload (very high distinct-queryid cardinality with a long
// CascadeCooldownCycles) cannot grow it without bound between
// cycles. When the cap is exceeded, the entry with the smallest
// remaining cooldown is evicted — it is closest to expiry anyway.
const maxRecentlyTuned = 10_000

// recordTuned inserts a queryID into the cooldown map, evicting the
// entry nearest to expiry if the cap would be exceeded.
func (t *Tuner) recordTuned(queryID int64) {
	if len(t.recentlyTuned) >= maxRecentlyTuned {
		var evictID int64
		minRemaining := -1
		for qid, remaining := range t.recentlyTuned {
			if minRemaining == -1 || remaining < minRemaining {
				minRemaining = remaining
				evictID = qid
			}
		}
		delete(t.recentlyTuned, evictID)
	}
	t.recentlyTuned[queryID] = t.cooldownCycles()
}

// deferralReason returns a non-empty string when the candidate
// should be skipped because at least one of the relations its
// plan touches has a pending index optimizer recommendation.
// The string identifies the matched relation for logging.
func (t *Tuner) deferralReason(
	ctx context.Context,
	c candidate,
	deferredTables map[string]bool,
) string {
	if len(deferredTables) == 0 {
		return ""
	}
	planJSON := t.fetchPlanJSON(ctx, c.QueryID)
	if planJSON == "" {
		return ""
	}
	reason, err := matchDeferred([]byte(planJSON), deferredTables)
	if err != nil {
		t.logFn("WARN",
			"tuner: extract relations queryid=%d: %v",
			c.QueryID, err)
		return ""
	}
	return reason
}

// matchDeferred is the pure form of the deferral check: given a
// plan JSON and a deferred-tables set, it returns a non-empty
// reason string when any relation in the plan is in the set.
// Pulled out so the gating logic is testable without a database.
func matchDeferred(
	planJSON []byte, deferredTables map[string]bool,
) (string, error) {
	if len(deferredTables) == 0 || len(planJSON) == 0 {
		return "", nil
	}
	rels, err := ExtractRelations(planJSON)
	if err != nil {
		return "", err
	}
	for rel := range rels {
		if deferredTables[rel] {
			return "table " + rel + " has pending index recommendation", nil
		}
	}
	return "", nil
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
	for key, remaining := range t.llmPrescriptionCooldown {
		remaining--
		if remaining <= 0 {
			delete(t.llmPrescriptionCooldown, key)
		} else {
			t.llmPrescriptionCooldown[key] = remaining
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

	// Split stale_stats symptoms into separate table-level
	// ANALYZE findings so they bypass the query-hint flow.
	var staleFindings []analyzer.Finding
	symptoms, staleFindings = t.extractStaleStats(symptoms)

	if len(symptoms) == 0 {
		return staleFindings
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
	finding := t.buildFinding(ctx, c, symptoms, combined,
		title, rationale, rewrite, rewriteRationale)
	return append(staleFindings, finding)
}

// extractStaleStats splits a symptom list into the non-stale
// tail (returned first) and one ANALYZE finding per distinct
// stale table, deduping against t.staleStatsEmitted so the
// same table never produces two findings in one cycle.
func (t *Tuner) extractStaleStats(
	symptoms []PlanSymptom,
) ([]PlanSymptom, []analyzer.Finding) {
	var kept []PlanSymptom
	var findings []analyzer.Finding
	for _, s := range symptoms {
		if s.Kind != SymptomStaleStats {
			kept = append(kept, s)
			continue
		}
		schema := s.Schema
		table := s.RelationName
		if schema == "" {
			schema = "public"
		}
		canonical := schema + "." + table
		if t.staleStatsEmitted[canonical] {
			continue
		}
		t.staleStatsEmitted[canonical] = true

		sizeMB := int64(0)
		if t.staleStats != nil {
			sizeMB = t.staleStats.SizeMB(canonical)
		}
		findings = append(findings, analyzer.Finding{
			Category:         "stale_statistics",
			Severity:         "warning",
			ObjectType:       "table",
			ObjectIdentifier: canonical,
			Title: fmt.Sprintf(
				"Stale statistics on %s — ANALYZE recommended",
				canonical,
			),
			Detail: map[string]any{
				"canonical":  canonical,
				"trigger":    "nested_loop_misestimate",
				"size_mb":    sizeMB,
				"schemaname": schema,
				"tablename":  table,
			},
			Recommendation: fmt.Sprintf(
				"Run ANALYZE on %s to refresh optimizer statistics",
				canonical,
			),
			ActionRisk: "safe",
			RecommendedSQL: fmt.Sprintf(
				"ANALYZE %s", QuoteQualified(schema, table),
			),
		})
	}
	return kept, findings
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
	contextKey := llmContextFingerprint(c, planJSON, symptoms, fallbackHint)
	if t.llmSuppressionActive(ctx, contextKey) {
		t.logFn("tuner",
			"suppressing repeated LLM attempt for queryid %d",
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
		t.recordLLMSuppression(ctx, c, contextKey, "",
			"llm_error", err.Error())
		return nil
	}
	if len(rx) > 0 {
		rx = t.filterRepeatedLLMPrescriptions(ctx, c, planJSON,
			contextKey, rx)
	}
	if len(rx) == 0 {
		t.recordLLMSuppression(ctx, c, contextKey, "",
			"empty_or_duplicate", "LLM returned no usable prescription")
	}
	if len(rx) > 0 {
		t.logFn("tuner",
			"LLM-enhanced hints for queryid %d: %s",
			c.QueryID, rx[0].HintDirective)
	}
	return rx
}

func (t *Tuner) filterRepeatedLLMPrescriptions(
	ctx context.Context,
	c candidate,
	planJSON string,
	contextKey string,
	prescriptions []Prescription,
) []Prescription {
	var kept []Prescription
	for _, p := range prescriptions {
		key := llmPrescriptionFingerprint(c, planJSON, p)
		if t.llmPrescriptionCooldown[key] > 0 ||
			t.llmSuppressionActive(ctx, key) {
			t.logFn("tuner",
				"suppressing repeated LLM prescription for queryid %d",
				c.QueryID)
			t.recordLLMSuppression(ctx, c, contextKey, key,
				"duplicate_prescription",
				"same LLM prescription is still cooling down")
			continue
		}
		t.llmPrescriptionCooldown[key] = t.cooldownCycles()
		t.recordLLMSuppression(ctx, c, contextKey, key,
			"accepted", "accepted LLM prescription cooldown")
		kept = append(kept, p)
	}
	return kept
}

func llmContextFingerprint(
	c candidate,
	planJSON string,
	symptoms []PlanSymptom,
	fallbackHint string,
) string {
	parts := []string{
		fmt.Sprintf("%d", c.QueryID),
		normalizeFingerprintText(c.Query),
		normalizeFingerprintText(planJSON),
		normalizeFingerprintText(fallbackHint),
	}
	for _, s := range symptoms {
		parts = append(parts, normalizeFingerprintText(string(s.Kind)))
		parts = append(parts, normalizeFingerprintText(s.RelationName))
		parts = append(parts, normalizeFingerprintText(s.Schema))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

func llmPrescriptionFingerprint(
	c candidate,
	planJSON string,
	p Prescription,
) string {
	normalized := strings.Join([]string{
		fmt.Sprintf("%d", c.QueryID),
		normalizeFingerprintText(c.Query),
		normalizeFingerprintText(planJSON),
		normalizeFingerprintText(p.HintDirective),
		normalizeFingerprintText(p.SuggestedRewrite),
	}, "\n")
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

func normalizeFingerprintText(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

const llmSuppressionCategory = "query_tuning"
const llmSuppressionStatus = "suppressed"

type llmSuppressionDetail struct {
	QueryID                    int64  `json:"queryid"`
	Query                      string `json:"query"`
	LLMContextFingerprint      string `json:"llm_context_fingerprint"`
	LLMPrescriptionFingerprint string `json:"llm_prescription_fingerprint"`
	LLMOutcome                 string `json:"llm_outcome"`
	Reason                     string `json:"reason"`
}

func (t *Tuner) llmSuppressionActive(
	ctx context.Context,
	fingerprint string,
) bool {
	if t.pool == nil || fingerprint == "" {
		return false
	}
	var active bool
	err := t.pool.QueryRow(ctx,
		`SELECT EXISTS (
		    SELECT 1 FROM sage.findings
		     WHERE category = $1
		       AND status = $2
		       AND suppressed_until > now()
		       AND (
		           detail->>'llm_context_fingerprint' = $3
		           OR detail->>'llm_prescription_fingerprint' = $3
		       )
		)`,
		llmSuppressionCategory, llmSuppressionStatus, fingerprint,
	).Scan(&active)
	if err != nil {
		t.logFn("WARN", "tuner: check LLM suppression: %v", err)
		return false
	}
	return active
}

func (t *Tuner) recordLLMSuppression(
	ctx context.Context,
	c candidate,
	contextKey string,
	prescriptionKey string,
	outcome string,
	reason string,
) {
	if t.pool == nil || contextKey == "" {
		return
	}
	detail := llmSuppressionDetail{
		QueryID:                    c.QueryID,
		Query:                      c.Query,
		LLMContextFingerprint:      contextKey,
		LLMPrescriptionFingerprint: prescriptionKey,
		LLMOutcome:                 outcome,
		Reason:                     reason,
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		t.logFn("WARN", "tuner: encode LLM suppression: %v", err)
		return
	}
	objectID := "llm_suppression:" + contextKey
	suppressedUntil := time.Now().UTC().Add(t.llmSuppressionDuration())
	updated, err := t.updateLLMSuppression(
		ctx, objectID, detailJSON, outcome, suppressedUntil)
	if err != nil {
		t.logFn("WARN", "tuner: update LLM suppression: %v", err)
		return
	}
	if updated {
		return
	}
	if err := t.insertLLMSuppression(
		ctx, objectID, detailJSON, outcome, suppressedUntil); err != nil {
		t.logFn("WARN", "tuner: insert LLM suppression: %v", err)
	}
}

func (t *Tuner) updateLLMSuppression(
	ctx context.Context,
	objectID string,
	detailJSON []byte,
	outcome string,
	suppressedUntil time.Time,
) (bool, error) {
	tag, err := t.pool.Exec(ctx,
		`UPDATE sage.findings
		    SET last_seen = now(),
		        detail = $3,
		        recommendation = $4,
		        suppressed_until = $5
		  WHERE category = $1
		    AND object_identifier = $2
		    AND status = 'suppressed'`,
		llmSuppressionCategory, objectID, detailJSON,
		"LLM tuner attempt suppressed: "+outcome, suppressedUntil,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (t *Tuner) insertLLMSuppression(
	ctx context.Context,
	objectID string,
	detailJSON []byte,
	outcome string,
	suppressedUntil time.Time,
) error {
	_, err := t.pool.Exec(ctx,
		`INSERT INTO sage.findings
		    (category, severity, object_type, object_identifier,
		     title, detail, recommendation, status, suppressed_until)
		 VALUES ($1, 'info', 'query', $2, $3, $4, $5, $6, $7)`,
		llmSuppressionCategory, objectID,
		"Suppressed repeated LLM tuner attempt",
		detailJSON, "LLM tuner attempt suppressed: "+outcome,
		llmSuppressionStatus, suppressedUntil,
	)
	return err
}

func (t *Tuner) llmSuppressionDuration() time.Duration {
	return time.Duration(t.cooldownCycles()) * time.Hour
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
	// Route bad_nested_loop to stale_stats symptoms when any
	// relation in the plan has stale statistics.
	symptoms = annotateForStaleStats(
		[]byte(planJSON), symptoms, t.staleStats,
	)
	return symptoms
}

func (t *Tuner) fetchPlanJSON(
	ctx context.Context, queryID int64,
) string {
	if t.pool == nil {
		return ""
	}
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
	ctx context.Context,
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
			c.Query, combinedHint,
		)
		f.RollbackSQL = BuildDeleteSQL(c.Query)
	}

	// Persist to sage.query_hints for the dashboard query-hints page.
	t.upsertQueryHint(ctx, c.QueryID, combinedHint,
		strings.Join(names, ", "), suggestedRewrite, rewriteRationale)

	return f
}

// upsertQueryHint writes a record to sage.query_hints so the
// dashboard query-hints page displays tuner findings.
func (t *Tuner) upsertQueryHint(
	ctx context.Context,
	queryID int64, hintText, symptom,
	suggestedRewrite, rewriteRationale string,
) {
	if t.pool == nil {
		return
	}
	// Update existing active hint, or insert new one.
	tag, err := t.pool.Exec(ctx,
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
		_, err = t.pool.Exec(ctx,
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
//
// The SQL this produces is stored as Finding.RecommendedSQL and
// executed later by the executor (see executor.ExecInTransaction),
// so it must be self-contained — we cannot use bind parameters.
//
// Safety: we emit the query text and hint as PostgreSQL dollar-
// quoted string literals ($sageqh$…$sageqh$). Dollar-quoted
// strings do not interpret backslash escapes or double single
// quotes, so they are immune to single-quote-based injection
// regardless of standard_conforming_strings. The only attack
// surface is a literal that itself contains the tag; we pick a
// unique tag for each call to eliminate that case and strip NUL
// bytes as extra defense-in-depth.
func BuildInsertSQL(queryText string, hint string) string {
	queryText = stripNULs(queryText)
	hint = stripNULs(hint)
	tag := chooseDollarTag(queryText, hint)
	return fmt.Sprintf(
		"INSERT INTO hint_plan.hints "+
			"(norm_query_string, application_name, hints) "+
			"VALUES (%s%s%s, '', %s%s%s) "+
			"ON CONFLICT (norm_query_string, application_name) "+
			"DO UPDATE SET hints = EXCLUDED.hints",
		tag, queryText, tag, tag, hint, tag,
	)
}

// BuildDeleteSQL generates a DELETE for hint_plan.hints.
// See BuildInsertSQL for the dollar-quoting safety rationale.
func BuildDeleteSQL(queryText string) string {
	queryText = stripNULs(queryText)
	tag := chooseDollarTag(queryText, "")
	return fmt.Sprintf(
		"DELETE FROM hint_plan.hints "+
			"WHERE norm_query_string = %s%s%s "+
			"AND application_name = ''",
		tag, queryText, tag,
	)
}

// stripNULs removes NUL bytes, which PostgreSQL rejects in text
// columns but which could truncate client-side string handling.
func stripNULs(s string) string {
	return strings.ReplaceAll(s, "\x00", "")
}

// chooseDollarTag picks a dollar-quote tag guaranteed not to
// appear in any of the provided payloads, so the closing tag is
// unambiguous. It starts with $sageqh$ and extends with digits
// until the tag is not a substring of any payload.
func chooseDollarTag(payloads ...string) string {
	base := "sageqh"
	for i := 0; i < 1000; i++ {
		var tag string
		if i == 0 {
			tag = "$" + base + "$"
		} else {
			tag = fmt.Sprintf("$%s%d$", base, i)
		}
		collides := false
		for _, p := range payloads {
			if strings.Contains(p, tag) {
				collides = true
				break
			}
		}
		if !collides {
			return tag
		}
	}
	// Extremely unlikely fallback — caller payloads are from
	// pg_stat_statements and unlikely to collide with 1000
	// different candidate tags.
	return "$sageqh_fallback$"
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
