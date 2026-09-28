package rca

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/sre"
)

// Incident narration (Sage SRE M0). Detection never depends on the LLM:
// every incident_detected / incident_escalated notification carries the
// deterministic summary, and rca.narration_enabled lets the LLM rewrite
// it, citing only the incident's own evidence, through ChatWithTools.
// Any failure (off, budget, rate limit, timeout, malformed or uncited
// output) falls back to the deterministic summary, labeled as such.

// Narration sources.
const (
	NarrationLLM           = "llm"
	NarrationDeterministic = "deterministic"
)

// Budget for one narration (AI-SRE-SPEC §11: 2 model turns, 16k input
// and 4k output tokens) and for one persistence cycle.
const (
	narrationMaxTurns          = 2
	narrationMaxTokens         = 1024
	narrationInputBudgetTokens = 16000
	narrationTurnTimeout       = 20 * time.Second
	narrationBatchTimeout      = 45 * time.Second
	narrationBatchLimit        = 3
	maxNarrativeRunes          = 1200
	maxDeterministicLinks      = 4
)

// Narration is the summary attached to an incident notification.
type Narration struct {
	Text           string
	Source         string
	Citations      []string
	FallbackReason string
	llmAttempted   bool
}

// DeterministicNarration summarizes an incident from its root cause and
// first evidence links. It never calls the LLM.
func DeterministicNarration(inc Incident) Narration {
	var b strings.Builder
	root := inc.RootCause
	if root == "" {
		root = "Incident detected"
	}
	b.WriteString(root)
	for i, l := range inc.CausalChain {
		if i == maxDeterministicLinks {
			break
		}
		fmt.Fprintf(&b, "; E%d: %s", i+1, linkText(l))
	}
	return Narration{Text: truncateRunes(b.String(), 2*maxNarrativeRunes),
		Source: NarrationDeterministic}
}

func (n Narration) fallback(reason string) Narration {
	n.Source = NarrationDeterministic
	n.FallbackReason = reason
	n.Citations = nil
	return n
}

func linkText(l ChainLink) string {
	if l.Evidence == "" {
		return l.Description
	}
	return l.Description + " (" + l.Evidence + ")"
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}

// narrate returns the LLM narration of inc, or the deterministic one with
// the reason the LLM was not used. It never holds e.mu during I/O.
func (e *Engine) narrate(ctx context.Context, inc Incident) Narration {
	ev := e.gatherEvidence(ctx, inc)
	det := deterministicNarration(inc, ev)
	e.mu.Lock()
	client, enabled := e.llmClient, e.cfg.NarrationEnabled
	e.mu.Unlock()
	if !enabled {
		return det.fallback("narration disabled")
	}
	if client == nil || !client.IsEnabled() {
		return det.fallback("llm unavailable")
	}
	det.llmAttempted = true
	n, err := runNarration(ctx, client, inc, ev)
	if err != nil {
		if !client.IsEnabled() {
			return det.fallback("llm unavailable (disabled during narration)")
		}
		return det.fallback(narrationFailure(err))
	}
	n.llmAttempted = true
	return n
}

var (
	errNarrationBudget    = errors.New("input budget exceeded")
	errNarrationTurnLimit = errors.New("tool turn limit reached")
)

func narrationFailure(err error) string {
	switch {
	case errors.Is(err, llm.ErrRateLimited):
		return "llm rate limited"
	case errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, context.Canceled):
		return "llm timeout"
	case errors.Is(err, llm.ErrEmptyResponse):
		return "llm empty response"
	case errors.Is(err, llm.ErrMalformedToolCall):
		return "llm malformed tool call"
	default:
		return truncateRunes(err.Error(), 200)
	}
}

// runNarration drives at most narrationMaxTurns tool-calling turns. The
// last turn forbids tools; tool calls there end the narration.
func runNarration(
	ctx context.Context, client *llm.Client, inc Incident, probeEv incidentEvidence,
) (Narration, error) {
	ev := newEvidenceSet(inc, probeEv)
	msgs := narrationPrompt(inc, ev)
	for turn := 1; turn <= narrationMaxTurns; turn++ {
		if estimateMessageTokens(msgs) > narrationInputBudgetTokens {
			return Narration{}, errNarrationBudget
		}
		choice := llm.ToolChoiceAuto
		if turn == narrationMaxTurns {
			choice = llm.ToolChoiceNone
		}
		res, err := client.ChatWithTools(ctx, msgs, narrationTools,
			llm.ToolOptions{MaxTokens: narrationMaxTokens,
				Timeout: narrationTurnTimeout, ToolChoice: choice})
		if err != nil {
			return Narration{}, err
		}
		if len(res.ToolCalls) == 0 {
			return parseNarration(res.Content, ev)
		}
		msgs = append(msgs, llm.Message{Role: "assistant",
			Content: res.Content, ToolCalls: res.ToolCalls})
		for _, call := range res.ToolCalls {
			msgs = append(msgs, llm.Message{Role: "tool",
				ToolCallID: call.ID, Content: ev.run(call)})
		}
	}
	return Narration{}, errNarrationTurnLimit
}

func estimateMessageTokens(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
		for _, c := range m.ToolCalls {
			n += len(c.Arguments)
		}
	}
	return n / 4
}

// parseNarration accepts only a complete JSON object (fences allowed, no
// truncation repair): {"claims": [{"text", "evidence_ids"}]}, or the M0
// form {"summary", "evidence_ids"} as a single claim. Every claim must
// cite known evidence (E#, P#, H#), and every number in a claim must
// appear in the evidence that claim cites (sre.ValidateClaims).
func parseNarration(content string, ev evidenceSet) (Narration, error) {
	var out struct {
		Claims      []sre.Claim `json:"claims"`
		Summary     string      `json:"summary"`
		EvidenceIDs []string    `json:"evidence_ids"`
	}
	cleaned := llm.StripJSON(content, llm.JSONObject)
	if err := json.Unmarshal([]byte(cleaned), &out); err != nil {
		return Narration{}, errors.New("malformed narration JSON")
	}
	claims := out.Claims
	if claims == nil {
		summary := strings.TrimSpace(out.Summary)
		if summary == "" {
			return Narration{}, errors.New("invalid summary: empty")
		}
		if utf8.RuneCountInString(summary) > maxNarrativeRunes {
			return Narration{}, fmt.Errorf("invalid summary: longer than %d "+
				"characters", maxNarrativeRunes)
		}
		claims = []sre.Claim{{Text: summary, EvidenceIDs: out.EvidenceIDs}}
	}
	if err := sre.ValidateClaims(claims, ev.catalog()); err != nil {
		return Narration{}, err
	}
	texts := make([]string, len(claims))
	for i, c := range claims {
		texts[i] = strings.TrimSpace(c.Text)
	}
	text := strings.Join(texts, " ")
	if utf8.RuneCountInString(text) > maxNarrativeRunes {
		return Narration{}, fmt.Errorf("invalid summary: longer than %d "+
			"characters", maxNarrativeRunes)
	}
	return Narration{Text: text, Source: NarrationLLM,
		Citations: sre.Citations(claims)}, nil
}

// pendingEvent is a notification that became due, with the incident it
// describes (for narration).
type pendingEvent struct {
	event    notify.Event
	incident Incident
}

func narratedEvent(typ string) bool {
	return typ == "incident_detected" || typ == "incident_escalated"
}

// decorateEvents attaches a narration to detected and escalated events.
// At most narrationBatchLimit LLM narrations run per call, all within
// narrationBatchTimeout; the rest use the deterministic summary.
func (e *Engine) decorateEvents(
	ctx context.Context, pending []pendingEvent,
) []notify.Event {
	ctx, cancel := context.WithTimeout(ctx, narrationBatchTimeout)
	defer cancel()
	out := make([]notify.Event, 0, len(pending))
	attempts := 0
	for _, p := range pending {
		if !narratedEvent(p.event.Type) {
			out = append(out, p.event)
			continue
		}
		var n Narration
		if attempts >= narrationBatchLimit {
			n = DeterministicNarration(p.incident).fallback(
				"cycle narration limit reached")
		} else {
			n = e.narrate(ctx, p.incident)
		}
		if n.llmAttempted {
			attempts++
		}
		out = append(out, withNarration(p.event, n))
	}
	return out
}

func withNarration(ev notify.Event, n Narration) notify.Event {
	data := make(map[string]any, len(ev.Data)+4)
	for k, v := range ev.Data {
		data[k] = v
	}
	data["narrative"] = n.Text
	data["narrative_source"] = n.Source
	label := "deterministic"
	if n.Source == NarrationLLM {
		data["narrative_citations"] = n.Citations
		label = "LLM, cites " + strings.Join(n.Citations, ", ")
	}
	if n.FallbackReason != "" {
		data["narrative_fallback_reason"] = n.FallbackReason
	}
	ev.Data = data
	ev.Body += "\n\nSummary (" + label + "): " + n.Text
	return ev
}
