package rca

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// tier2Response is the expected JSON structure from the LLM.
type tier2Response struct {
	RootCause      string   `json:"root_cause"`
	Severity       string   `json:"severity"`
	CausalChain    string   `json:"causal_chain"`
	RecommendedSQL []string `json:"recommended_sql"`
	ActionRisk     string   `json:"action_risk"`
}

// tier2LLMTimeout is the context deadline for a single Tier 2 call.
const tier2LLMTimeout = 30 * time.Second

// tier2DefaultConfidence is the confidence score for LLM incidents.
const tier2DefaultConfidence = 0.6

// runTier2Correlation identifies uncovered signals and, when the
// threshold is met and an LLM client is available, asks the LLM to
// correlate them into an incident.
func (e *Engine) runTier2Correlation(
	signals []*Signal,
	tier1Incidents []Incident,
) []Incident {
	if e.llmClient == nil || !e.llmClient.IsEnabled() {
		return nil
	}

	uncovered := findUncoveredSignals(signals, tier1Incidents)
	threshold := e.cfg.LLMCorrelationThreshold
	if threshold == 0 {
		threshold = 3
	}
	if len(uncovered) < threshold {
		return nil
	}

	return e.callTier2LLM(uncovered)
}

// findUncoveredSignals returns signals whose IDs were not consumed
// by any Tier 1 incident.
func findUncoveredSignals(
	signals []*Signal,
	incidents []Incident,
) []*Signal {
	covered := make(map[string]bool)
	for _, inc := range incidents {
		for _, sid := range inc.SignalIDs {
			covered[sid] = true
		}
	}
	var uncovered []*Signal
	for _, s := range signals {
		if !covered[s.ID] {
			uncovered = append(uncovered, s)
		}
	}
	return uncovered
}

// callTier2LLM sends uncovered signals to the LLM and parses the
// response into an Incident. Returns nil on any error.
func (e *Engine) callTier2LLM(uncovered []*Signal) []Incident {
	ctx, cancel := context.WithTimeout(
		context.Background(), tier2LLMTimeout)
	defer cancel()

	system := buildTier2SystemPrompt()
	user := buildTier2UserPrompt(uncovered)

	raw, _, err := e.llmClient.Chat(ctx, system, user, 2048)
	if err != nil {
		e.logFn("warn", "rca: tier2 LLM call failed: %v", err)
		return nil
	}

	inc, err := parseTier2Response(raw, uncovered)
	if err != nil {
		e.logFn("warn", "rca: tier2 parse failed: %v", err)
		return nil
	}
	return []Incident{inc}
}

// buildTier2SystemPrompt returns the system message for Tier 2.
func buildTier2SystemPrompt() string {
	return "You are a PostgreSQL root cause analysis engine. " +
		"You are given N signals that fired simultaneously " +
		"but do not match any known deterministic pattern. " +
		"Correlate the signals, identify the most likely " +
		"root cause, and return ONLY a JSON object with " +
		"these fields: root_cause (string), severity " +
		"(\"warning\" or \"critical\"), causal_chain " +
		"(string, use arrow notation A -> B -> C), " +
		"recommended_sql (array of SQL strings, may be " +
		"empty), action_risk (\"low\", \"medium\", or " +
		"\"high\"). No markdown fences. No extra text."
}

// buildTier2UserPrompt formats uncovered signals for the LLM.
func buildTier2UserPrompt(uncovered []*Signal) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d uncovered signals fired:\n\n",
		len(uncovered))
	for i, s := range uncovered {
		metricsJSON, _ := json.Marshal(s.Metrics)
		fmt.Fprintf(&b, "%d. ID=%s severity=%s fired_at=%s "+
			"metrics=%s\n",
			i+1, s.ID, s.Severity,
			s.FiredAt.Format(time.RFC3339), string(metricsJSON))
	}
	return b.String()
}

// parseTier2Response extracts a tier2Response from possibly
// markdown-fenced LLM output and builds an Incident.
func parseTier2Response(
	raw string,
	uncovered []*Signal,
) (Incident, error) {
	cleaned := stripToJSONObject(raw)
	var resp tier2Response
	if err := json.Unmarshal([]byte(cleaned), &resp); err != nil {
		return Incident{}, fmt.Errorf("unmarshal: %w", err)
	}
	if resp.RootCause == "" {
		return Incident{}, fmt.Errorf("empty root_cause in response")
	}

	return buildTier2Incident(resp, uncovered), nil
}

// buildTier2Incident constructs an Incident from the parsed LLM
// response and the uncovered signals that produced it.
func buildTier2Incident(
	resp tier2Response,
	uncovered []*Signal,
) Incident {
	ids := make([]string, len(uncovered))
	for i, s := range uncovered {
		ids[i] = s.ID
	}

	severity := resp.Severity
	if severity != "warning" && severity != "critical" {
		severity = "warning"
	}
	risk := resp.ActionRisk
	if risk != "low" && risk != "medium" && risk != "high" {
		risk = "medium"
	}

	chain := parseCausalChainString(resp.CausalChain, uncovered)
	sql := strings.Join(resp.RecommendedSQL, "; ")

	return Incident{
		DetectedAt:      time.Now(),
		Severity:        severity,
		RootCause:       resp.RootCause,
		CausalChain:     chain,
		AffectedObjects: nil,
		SignalIDs:       ids,
		RecommendedSQL:  sql,
		ActionRisk:      risk,
		Source:          "llm",
		Confidence:      tier2DefaultConfidence,
	}
}

// parseCausalChainString converts "A -> B -> C" into ChainLinks.
func parseCausalChainString(
	raw string,
	uncovered []*Signal,
) []ChainLink {
	// Split on various arrow notations.
	parts := strings.Split(raw, "->")
	if len(parts) <= 1 {
		parts = strings.Split(raw, "\u2192") // unicode arrow
	}
	var chain []ChainLink
	for i, part := range parts {
		desc := strings.TrimSpace(part)
		if desc == "" {
			continue
		}
		sig := ""
		if i < len(uncovered) {
			sig = uncovered[i].ID
		}
		chain = append(chain, ChainLink{
			Order:       i + 1,
			Signal:      sig,
			Description: desc,
		})
	}
	return chain
}

// stripToJSONObject extracts a JSON object from text that may
// contain markdown fences or thinking tokens.
func stripToJSONObject(s string) string {
	s = strings.TrimSpace(s)
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	// Fallback: strip markdown fences.
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
