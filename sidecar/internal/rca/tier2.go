package rca

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
)

// tier2Response is the expected JSON structure from the LLM.
// causal_steps is preferred; causal_chain (arrow notation) is accepted
// for backward compatibility.
type tier2Response struct {
	RootCause      string      `json:"root_cause"`
	Severity       string      `json:"severity"`
	CausalChain    string      `json:"causal_chain"`
	CausalSteps    []tier2Step `json:"causal_steps"`
	RecommendedSQL []string    `json:"recommended_sql"`
	ActionRisk     string      `json:"action_risk"`
}

// tier2Step is one causal step; Signal must be one of the input IDs.
type tier2Step struct {
	Signal      string `json:"signal"`
	Description string `json:"description"`
}

// tier2LLMTimeout is the upper bound for a single Tier 2 call; the caller
// context may end it sooner.
const tier2LLMTimeout = 30 * time.Second

// tier2DefaultConfidence is the confidence score for LLM incidents.
const tier2DefaultConfidence = 0.6

// tier2LowConfidence marks deterministic incidents weak enough that their
// signals are also offered to Tier 2 for correlation.
const tier2LowConfidence = 0.7

// tier2Request is a Tier 2 call planned under e.mu and executed without
// it. candidates are the signals Tier 1 did not explain (the identity of
// the resulting incident); context is every signal of the cycle.
type tier2Request struct {
	client     *llm.Client
	candidates []*Signal
	context    []*Signal
	database   string
}

// planTier2 decides whether Tier 2 should run. It fires when at least
// llm_correlation_threshold signals co-occur and at least one of them is
// unexplained (no Tier 1 tree consumed it, or only a low-confidence one
// did). Before, it required threshold *unexplained* signals, which the
// production signal set can never produce (substrate-B2). If an open LLM
// incident already covers the same unexplained signals, no call is made
// and the incident is re-observed instead (G3-B17). Caller holds e.mu.
func (e *Engine) planTier2(
	signals []*Signal, tier1 []Incident,
) (*tier2Request, []Incident) {
	if e.llmClient == nil || !e.llmClient.IsEnabled() {
		return nil, nil
	}
	threshold := e.cfg.LLMCorrelationThreshold
	if threshold <= 0 {
		threshold = 3
	}
	candidates := tier2Candidates(signals, tier1)
	if len(candidates) == 0 || len(signals) < threshold {
		return nil, nil
	}
	probe := Incident{
		Source: "llm", DatabaseName: e.databaseName,
		SignalIDs: signalIDs(candidates),
	}
	key := identityString(&probe)
	for i := range e.incidents {
		existing := e.incidents[i]
		if existing.ResolvedAt == nil && identityString(&existing) == key {
			existing.DetectedAt = time.Now()
			return nil, []Incident{existing}
		}
	}
	return &tier2Request{
		client: e.llmClient, candidates: candidates,
		context: signals, database: e.databaseName,
	}, nil
}

// tier2Candidates returns signals not consumed by any Tier 1 incident,
// plus signals consumed only by low-confidence incidents.
func tier2Candidates(signals []*Signal, tier1 []Incident) []*Signal {
	strong := make(map[string]bool)
	for _, inc := range tier1 {
		if inc.Confidence < tier2LowConfidence {
			continue
		}
		for _, sid := range inc.SignalIDs {
			strong[sid] = true
		}
	}
	var out []*Signal
	for _, s := range signals {
		if !strong[s.ID] {
			out = append(out, s)
		}
	}
	return out
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

func signalIDs(signals []*Signal) []string {
	ids := make([]string, len(signals))
	for i, s := range signals {
		ids[i] = s.ID
	}
	return ids
}

// runTier2 performs the LLM call outside the engine lock, bounded by the
// caller context (G3-B17). Returns nil on any error.
func (e *Engine) runTier2(ctx context.Context, req *tier2Request) []Incident {
	if err := ctx.Err(); err != nil {
		e.logFn("warn", "rca: tier2 skipped: %v", err)
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, tier2LLMTimeout)
	defer cancel()

	user := buildTier2UserPrompt(req.candidates, req.context)
	raw, _, err := req.client.Chat(cctx, buildTier2SystemPrompt(), user, 2048)
	if err != nil {
		e.logFn("warn", "rca: tier2 LLM call failed: %v", err)
		return nil
	}
	var resp tier2Response
	if err := llm.ParseJSON(raw, llm.JSONObject, &resp); err != nil {
		e.logFn("warn", "rca: tier2 parse failed: %v", err)
		return nil
	}
	if resp.RootCause == "" {
		e.logFn("warn", "rca: tier2 parse failed: empty root_cause")
		return nil
	}
	inc := buildTier2IncidentFrom(resp, req.candidates, req.context)
	inc.DatabaseName = req.database
	return []Incident{inc}
}

// buildTier2SystemPrompt returns the system message for Tier 2.
func buildTier2SystemPrompt() string {
	return "You are a PostgreSQL root cause analysis engine. " +
		"You are given signals that fired together. Some are marked " +
		"UNEXPLAINED: no deterministic rule accounts for them. " +
		"Correlate the signals, identify the most likely root cause " +
		"of the unexplained ones, and return ONLY a JSON object with " +
		"these fields: root_cause (string), severity (\"warning\" or " +
		"\"critical\"), causal_steps (array of objects with " +
		"\"signal\" set to one of the given signal IDs or \"\" and " +
		"\"description\"), recommended_sql (array with at most one " +
		"read-only diagnostic SQL statement, may be empty), " +
		"action_risk (\"low\", \"medium\", or \"high\"). " +
		"No markdown fences. No extra text."
}

// buildTier2UserPrompt formats signals for the LLM, redacting free text
// that may carry data values or identities.
func buildTier2UserPrompt(candidates, all []*Signal) string {
	unexplained := make(map[string]bool, len(candidates))
	for _, s := range candidates {
		unexplained[s.ID] = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d uncovered signals fired among %d "+
		"co-occurring signals:\n\n", len(candidates), len(all))
	for i, s := range all {
		status := "explained"
		if unexplained[s.ID] {
			status = "UNEXPLAINED"
		}
		metricsJSON, err := json.Marshal(redactSignalMetrics(s.Metrics))
		if err != nil {
			metricsJSON = []byte(`{}`)
		}
		fmt.Fprintf(&b, "%d. ID=%s status=%s severity=%s fired_at=%s "+
			"metrics=%s\n", i+1, s.ID, status, s.Severity,
			s.FiredAt.Format(time.RFC3339), string(metricsJSON))
	}
	return b.String()
}

// droppedPromptMetrics never leave the sidecar: DETAIL lines carry key
// values ("Key (email)=(...)") and user names identify people.
var droppedPromptMetrics = map[string]bool{"detail": true, "user": true}

var doubleQuoted = regexp.MustCompile(`"[^"]*"`)

// redactSignalMetrics copies metrics for an LLM prompt: log messages have
// quoted values redacted and queries are comment-stripped and have
// literals redacted.
func redactSignalMetrics(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if droppedPromptMetrics[k] {
			continue
		}
		s, isString := v.(string)
		switch {
		case isString && k == "query":
			out[k] = llm.SanitizeForLLM(s)
		case isString && k == "message":
			out[k] = doubleQuoted.ReplaceAllString(
				llm.RedactSQLLiterals(s), `"?"`)
		default:
			out[k] = v
		}
	}
	return out
}

// parseTier2Response extracts a tier2Response from possibly
// markdown-fenced LLM output and builds an Incident.
func parseTier2Response(
	raw string,
	uncovered []*Signal,
) (Incident, error) {
	var resp tier2Response
	if err := llm.ParseJSON(raw, llm.JSONObject, &resp); err != nil {
		return Incident{}, err
	}
	if resp.RootCause == "" {
		return Incident{}, fmt.Errorf("empty root_cause in response")
	}
	return buildTier2Incident(resp, uncovered), nil
}

// buildTier2Incident constructs an Incident whose identity is the given
// signals and whose chain may reference only those signals.
func buildTier2Incident(resp tier2Response, uncovered []*Signal) Incident {
	return buildTier2IncidentFrom(resp, uncovered, uncovered)
}

// buildTier2IncidentFrom constructs an Incident for candidates; causal
// steps may reference any signal in known.
func buildTier2IncidentFrom(
	resp tier2Response, candidates, known []*Signal,
) Incident {
	severity := resp.Severity
	if severity != "warning" && severity != "critical" {
		severity = "warning"
	}
	risk := resp.ActionRisk
	if risk != "low" && risk != "medium" && risk != "high" {
		risk = "medium"
	}
	chain := chainFromSteps(resp.CausalSteps, known)
	if len(chain) == 0 {
		chain = parseCausalChainString(resp.CausalChain, known)
	}
	return Incident{
		DetectedAt:     time.Now(),
		Severity:       severity,
		RootCause:      resp.RootCause,
		CausalChain:    chain,
		SignalIDs:      signalIDs(candidates),
		RecommendedSQL: firstSingleStatement(resp.RecommendedSQL),
		ActionRisk:     risk,
		Source:         "llm",
		Confidence:     tier2DefaultConfidence,
	}
}

// chainFromSteps keeps a step's signal only when it names a real input
// signal (G3-B27).
func chainFromSteps(steps []tier2Step, known []*Signal) []ChainLink {
	ids := make(map[string]bool, len(known))
	for _, s := range known {
		ids[s.ID] = true
	}
	var chain []ChainLink
	for _, st := range steps {
		desc := strings.TrimSpace(st.Description)
		if desc == "" {
			continue
		}
		sig := ""
		if ids[st.Signal] {
			sig = st.Signal
		}
		chain = append(chain, ChainLink{
			Order: len(chain) + 1, Signal: sig, Description: desc,
		})
	}
	return chain
}

var signalToken = regexp.MustCompile(`[A-Za-z0-9_]+`)

// parseCausalChainString converts "A -> B -> C" into ChainLinks. A step is
// attributed to a signal only when its text names that signal ID; steps
// are never mapped to signals by position (G3-B27).
func parseCausalChainString(raw string, known []*Signal) []ChainLink {
	ids := make([]string, 0, len(known))
	for _, s := range known {
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)
	parts := strings.Split(raw, "->")
	if len(parts) <= 1 {
		parts = strings.Split(raw, "→") // unicode arrow
	}
	var chain []ChainLink
	for _, part := range parts {
		desc := strings.TrimSpace(part)
		if desc == "" {
			continue
		}
		chain = append(chain, ChainLink{
			Order:       len(chain) + 1,
			Signal:      namedSignal(desc, ids),
			Description: desc,
		})
	}
	return chain
}

// namedSignal returns the first token of desc that is a known signal ID.
func namedSignal(desc string, sortedIDs []string) string {
	for _, tok := range signalToken.FindAllString(desc, -1) {
		i := sort.SearchStrings(sortedIDs, tok)
		if i < len(sortedIDs) && sortedIDs[i] == tok {
			return tok
		}
	}
	return ""
}

// firstSingleStatement returns the first non-empty recommended statement
// with trailing semicolons removed. Statements are never joined, and a
// statement that itself contains ';' is rejected (G3-B27): LLM SQL is
// unvalidated and must not become a multi-statement batch.
func firstSingleStatement(stmts []string) string {
	for _, raw := range stmts {
		s := strings.TrimRight(strings.TrimSpace(raw), "; \t\r\n")
		if s == "" {
			continue
		}
		if strings.Contains(s, ";") {
			return ""
		}
		return s
	}
	return ""
}

// stripToJSONObject extracts a JSON object from text that may
// contain markdown fences or thinking tokens. Delegates to the
// canonical llm.StripJSON implementation.
func stripToJSONObject(s string) string {
	return llm.StripJSON(s, llm.JSONObject)
}
