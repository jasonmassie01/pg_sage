package migration

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/rca"
)

// llmDDLResponse is the expected JSON structure from the LLM when
// classifying an unrecognized DDL statement.
type llmDDLResponse struct {
	LockLevel       string  `json:"lock_level"`
	RequiresRewrite bool    `json:"requires_rewrite"`
	RiskScore       float64 `json:"risk_score"`
	SafeAlternative string  `json:"safe_alternative"`
	Explanation     string  `json:"explanation"`
	EstDurationSec  int     `json:"estimated_duration_seconds"`
}

// ddlLLMTimeout is the context deadline for a single DDL LLM call.
const ddlLLMTimeout = 30 * time.Second

// fallbackRuleID is the rule ID of a finding the LLM fallback raised.
const fallbackRuleID = "llm_ddl_fallback"

// fallbackResult assesses DDL that no deterministic rule matched, on the
// enrichment worker. It returns the incident to persist and the model's
// alternative, or nil when the statement may not be sent, the model
// failed, or the risk is at or below the threshold (graceful degradation).
func (a *Advisor) fallbackResult(
	ctx context.Context, sql string,
) (*rca.Incident, ScriptResult) {
	clean := sanitizeDDL(sql)
	if !a.fallbackEligible(clean) {
		return nil, ScriptResult{}
	}
	resp := a.fallbackResponse(ctx, clean)
	if resp == nil {
		return nil, ScriptResult{}
	}
	resp.RiskScore = math.Min(1, math.Max(0, resp.RiskScore))
	if resp.RiskScore <= incidentThreshold {
		return nil, ScriptResult{}
	}
	return a.buildLLMIncident(resp, clean)
}

// fallbackResponse is the model's parsed assessment of clean, cached for
// the statement and its table's schema. A failed call, and a reply that
// does not parse, is not cached, so the next detection asks again.
func (a *Advisor) fallbackResponse(ctx context.Context, clean string) *llmDDLResponse {
	schema := ""
	if target := statementTarget(clean); target.Table != "" && a.scriptGen != nil {
		schema = a.scriptGen.schemaText(ctx, target.Schema, target.Table)
	}
	key := enrichKey(fallbackRuleID, clean, schema)
	raw, cached := a.cache.get(key)
	if !cached {
		var err error
		if raw, err = a.llmFallback(ctx, clean); err != nil {
			a.logFn("warn", "migration: LLM DDL fallback failed: %v", err)
			return nil
		}
	}
	resp, err := parseDDLLLMResponse(raw)
	if err != nil {
		a.logFn("warn", "migration: LLM DDL parse failed: %v", err)
		return nil
	}
	if !cached {
		a.cache.put(key, raw)
	}
	return resp
}

// llmFallback asks the model to assess sanitized DDL.
func (a *Advisor) llmFallback(ctx context.Context, sql string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ddlLLMTimeout)
	defer cancel()

	system := buildDDLSystemPrompt()
	user := buildDDLUserPrompt(sql, a.pgVersion, a.dbName)
	pctx := llm.WithPurpose(ctx, "migration_advice") // llm-04: usage ledger label
	raw, _, err := a.llmClient.Chat(pctx, system, user, 2048)
	return raw, err
}

// buildLLMIncident converts an llmDDLResponse into an rca.Incident with
// source "schema_advisor_llm" and no recommended SQL: the model's
// alternative is returned beside it, validated or labelled, for the
// llm_ detail keys (migration-10).
func (a *Advisor) buildLLMIncident(
	resp *llmDDLResponse, sql string,
) (*rca.Incident, ScriptResult) {
	severity := "warning"
	if resp.RiskScore > 0.7 {
		severity = "critical"
	}

	chain := []rca.ChainLink{
		{
			Order:       1,
			Signal:      "llm_ddl_analysis",
			Description: resp.Explanation,
			Evidence:    truncateSQL(sql, 200),
		},
		{
			Order:       2,
			Signal:      "lock_analysis",
			Description: fmt.Sprintf("Requires %s lock", resp.LockLevel),
			Evidence: fmt.Sprintf(
				"requires_rewrite=%v est_duration=%ds",
				resp.RequiresRewrite, resp.EstDurationSec),
		},
	}
	alt := a.validatedAlternative(resp.SafeAlternative, sql)
	switch {
	case alt.Script != "":
		chain = append(chain, rca.ChainLink{
			Order:       3,
			Signal:      detailLLMSafeAlternative,
			Description: validatedLabel,
			Evidence:    alt.Script,
		})
	case alt.ModelText != "":
		chain = append(chain, unvalidatedLink(3, alt.ModelText, alt.Reason))
	}

	return &rca.Incident{
		DetectedAt:   time.Now(),
		Severity:     severity,
		Source:       "schema_advisor_llm",
		RootCause:    fmt.Sprintf("LLM-detected risky DDL: %s", resp.Explanation),
		CausalChain:  chain,
		SignalIDs:    []string{fallbackRuleID},
		ActionRisk:   fmt.Sprintf("risk_score=%.2f", resp.RiskScore),
		Confidence:   resp.RiskScore,
		DatabaseName: a.dbName,
	}, alt
}

// validatedAlternative keeps the model's safe_alternative as Script only
// when it validates against the one table the original statement names
// (security-05).
func (a *Advisor) validatedAlternative(alt, original string) ScriptResult {
	alt = strings.TrimSpace(alt)
	if alt == "" {
		return ScriptResult{}
	}
	if err := validateModelDDL(alt, statementTarget(original)); err != nil {
		a.logFn("warn", "migration: LLM safe_alternative not validated: %v", err)
		return ScriptResult{ModelText: alt, Reason: err.Error()}
	}
	return ScriptResult{Script: alt}
}

// buildDDLSystemPrompt returns the system message for DDL analysis.
func buildDDLSystemPrompt() string {
	return "You are a PostgreSQL migration safety expert. " +
		"Analyze the given DDL statement and assess its risk. " +
		"Return ONLY a JSON object with these fields: " +
		"lock_level (the PostgreSQL lock level this DDL acquires, " +
		"e.g. \"ACCESS EXCLUSIVE\", \"SHARE\"), " +
		"requires_rewrite (boolean, whether this causes a table rewrite), " +
		"risk_score (float 0.0-1.0, considering lock level and rewrite), " +
		"safe_alternative (SQL for a safer approach, or empty string " +
		"if already safe), " +
		"explanation (one sentence explaining the risk), " +
		"estimated_duration_seconds (rough estimate for a 1M row table). " +
		"No markdown fences. No extra text.\n\n" + llm.UntrustedDataRule
}

// buildDDLUserPrompt formats the DDL statement for the LLM; the observed
// statement is untrusted, so it is fenced as data.
func buildDDLUserPrompt(sql string, pgVersion int, dbName string) string {
	return fmt.Sprintf(
		"PostgreSQL version: %s\nDatabase: %s\nDDL statement:\n%s",
		formatPGVersion(pgVersion), dbName,
		llm.UntrustedData("ddl", sanitizeDDL(sql)),
	)
}

// parseDDLLLMResponse extracts an llmDDLResponse from possibly
// markdown-fenced or truncated LLM output.
func parseDDLLLMResponse(raw string) (*llmDDLResponse, error) {
	var resp llmDDLResponse
	if err := llm.ParseJSON(raw, llm.JSONObject, &resp); err != nil {
		return nil, err
	}
	if resp.Explanation == "" {
		return nil, fmt.Errorf("empty explanation in LLM response")
	}
	return &resp, nil
}
