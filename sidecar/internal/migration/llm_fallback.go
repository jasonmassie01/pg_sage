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

// llmFallback asks the LLM to assess DDL that no deterministic rule
// matched. Returns nil on low-risk or any error (graceful degradation).
func (a *Advisor) llmFallback(
	ctx context.Context, sql string,
) (*rca.Incident, error) {
	ctx, cancel := context.WithTimeout(ctx, ddlLLMTimeout)
	defer cancel()

	system := buildDDLSystemPrompt()
	user := buildDDLUserPrompt(sql, a.pgVersion, a.dbName)

	raw, _, err := a.llmClient.Chat(ctx, system, user, 2048)
	if err != nil {
		a.logFn("warn",
			"migration: LLM DDL fallback failed: %v", err)
		return nil, nil
	}

	resp, err := parseDDLLLMResponse(raw)
	if err != nil {
		a.logFn("warn",
			"migration: LLM DDL parse failed: %v", err)
		return nil, nil
	}

	resp.RiskScore = math.Min(1, math.Max(0, resp.RiskScore))
	if resp.RiskScore <= incidentThreshold {
		return nil, nil
	}

	return a.buildLLMIncident(resp, sql), nil
}

// buildLLMIncident converts an llmDDLResponse into an rca.Incident
// with source "schema_advisor_llm".
func (a *Advisor) buildLLMIncident(
	resp *llmDDLResponse, sql string,
) *rca.Incident {
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
	recommended := a.validatedAlternative(resp.SafeAlternative, sql)
	switch {
	case recommended.SQL != "":
		chain = append(chain, rca.ChainLink{
			Order:       3,
			Signal:      "safe_alternative",
			Description: recommended.SQL,
		})
	case recommended.ModelText != "":
		chain = append(chain, unvalidatedLink(3, recommended.ModelText,
			recommended.Reason))
	}

	return &rca.Incident{
		DetectedAt:     time.Now(),
		Severity:       severity,
		Source:         "schema_advisor_llm",
		RootCause:      fmt.Sprintf("LLM-detected risky DDL: %s", resp.Explanation),
		CausalChain:    chain,
		SignalIDs:      []string{"llm_ddl_fallback"},
		RecommendedSQL: recommended.SQL,
		ActionRisk:     fmt.Sprintf("risk_score=%.2f", resp.RiskScore),
		Confidence:     resp.RiskScore,
		DatabaseName:   a.dbName,
	}
}

// validatedAlternative keeps the model's safe_alternative only when it
// validates against the one table the original statement names; there
// is no deterministic alternative to fall back to (security-05).
func (a *Advisor) validatedAlternative(alt, original string) ScriptResult {
	alt = strings.TrimSpace(alt)
	if alt == "" {
		return ScriptResult{}
	}
	if err := validateModelDDL(alt, statementTarget(original)); err != nil {
		a.logFn("warn", "migration: LLM safe_alternative not validated: %v", err)
		return ScriptResult{ModelText: alt, Reason: err.Error()}
	}
	return ScriptResult{SQL: alt}
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
