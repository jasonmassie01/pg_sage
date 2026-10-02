package tuner

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/llm"
)

const llmMaxTokens = 4096

// LLMPrescription is the structured response from the LLM.
type LLMPrescription struct {
	HintDirective    string  `json:"hint_directive"`
	Rationale        string  `json:"rationale"`
	Confidence       float64 `json:"confidence"`
	SuggestedRewrite string  `json:"suggested_rewrite"`
	RewriteRationale string  `json:"rewrite_rationale"`
}

// llmUsable reports whether client can serve a request now: configured
// (llm.enabled with endpoint and key), circuit closed, daily budget left.
func llmUsable(client *llm.Client) bool {
	return client != nil && client.IsEnabled() &&
		!client.IsCircuitOpen() && !client.IsBudgetExhausted()
}

// suppressesQuery reports whether a failed prescription is a verdict on
// the query. An empty completion (G3-B10) or a budget refusal is about
// the provider or the day, so suppressing would mute LLM tuning for it.
func suppressesQuery(err error) bool {
	return !errors.Is(err, llm.ErrEmptyResponse) &&
		!errors.Is(err, llm.ErrBudgetExhausted)
}

// llmPrescribe calls the LLM for hint reasoning, with fallback to a
// distinct client (a fallback that is the primary is not retried,
// G3-B15). workMemMaxMB bounds any Set(work_mem) the LLM proposes.
func llmPrescribe(
	ctx context.Context,
	client *llm.Client,
	fallback *llm.Client,
	qctx QueryContext,
	workMemMaxMB int,
	logFn func(string, string, ...any),
) ([]Prescription, error) {
	system := TunerSystemPrompt()
	prompt := FormatTunerPrompt(qctx)

	resp, _, err := client.Chat(ctx, system, prompt, llmMaxTokens)
	if err != nil && fallback != nil && fallback != client {
		logFn("tuner",
			"primary LLM failed, trying fallback: %v", err)
		resp, _, err = fallback.Chat(
			ctx, system, prompt, llmMaxTokens,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("llm chat: %w", err)
	}

	recs, err := parseLLMPrescriptions(resp)
	if err != nil {
		logFn("tuner",
			"LLM response parse error: %v (response: %.200s)",
			err, resp)
		return nil, fmt.Errorf("parse llm response: %w", err)
	}

	return convertPrescriptions(recs, workMemMaxMB, logFn), nil
}

// convertPrescriptions validates LLM hints: pg_hint_plan syntax, then
// Set() directives restricted to an allowlist with work_mem normalized to
// MB and clamped to workMemMaxMB (G3-B16).
func convertPrescriptions(
	recs []LLMPrescription,
	workMemMaxMB int,
	logFn func(string, string, ...any),
) []Prescription {
	var out []Prescription
	for _, r := range recs {
		if !validateHintSyntax(r.HintDirective) {
			logFn("tuner",
				"rejecting invalid LLM hint: %s", r.HintDirective)
			continue
		}
		hint, err := normalizeSetDirectives(r.HintDirective, workMemMaxMB)
		if err != nil {
			logFn("tuner", "rejecting LLM hint %s: %v", r.HintDirective, err)
			continue
		}
		out = append(out, Prescription{
			Symptom:          "llm_recommended",
			HintDirective:    hint,
			Rationale:        r.Rationale,
			SuggestedRewrite: r.SuggestedRewrite,
			RewriteRationale: r.RewriteRationale,
		})
	}
	return out
}

func parseLLMPrescriptions(
	response string,
) ([]LLMPrescription, error) {
	var recs []LLMPrescription
	if err := llm.ParseJSON(response, llm.JSONArray, &recs); err != nil {
		return nil, err
	}
	return recs, nil
}

// validHintTokens are the allowed pg_hint_plan directive prefixes.
var validHintTokens = []string{
	"Set(", "HashJoin(", "MergeJoin(", "NestLoop(",
	"IndexScan(", "IndexOnlyScan(", "SeqScan(", "NoSeqScan(",
	"Parallel(", "NoParallel(",
	"BitmapScan(", "NoBitmapScan(",
	"NoIndexScan(", "NoNestLoop(", "NoHashJoin(", "NoMergeJoin(",
}

// dangerousPatterns rejects SQL injection attempts.
var dangerousPatterns = regexp.MustCompile(
	`(?i)(;|--|\b(DROP|DELETE|INSERT|ALTER|CREATE|TRUNCATE|UPDATE|GRANT|REVOKE)\b)`,
)

// validateHintSyntax checks a hint string contains only valid
// pg_hint_plan tokens and no dangerous SQL.
func validateHintSyntax(hint string) bool {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return false
	}
	if dangerousPatterns.MatchString(hint) {
		return false
	}
	// Every non-whitespace token must start with a known prefix.
	// Split on ")  " boundaries to isolate directives.
	parts := splitHintDirectives(hint)
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !hasValidPrefix(p) {
			return false
		}
	}
	return true
}

func hasValidPrefix(s string) bool {
	for _, tok := range validHintTokens {
		if strings.HasPrefix(s, tok) {
			return true
		}
	}
	return false
}

// splitHintDirectives splits a combined hint string like
// "Set(work_mem \"256MB\") HashJoin(t1 t2)" into individual
// directive strings.
func splitHintDirectives(hint string) []string {
	var parts []string
	depth := 0
	start := 0
	for i, ch := range hint {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				parts = append(parts, hint[start:i+1])
				start = i + 1
			}
		}
	}
	// Trailing text (shouldn't happen in valid hints)
	if trail := strings.TrimSpace(hint[start:]); trail != "" {
		parts = append(parts, trail)
	}
	return parts
}
