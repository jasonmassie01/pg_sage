package agentloop

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/llm"
)

// The loop's own prompt text: the protocol, the budget, the seeded
// evidence, tool results (fenced as untrusted data) and corrections.

const nativeRules = "Investigate by calling the listed tools; you may call several in " +
	"one turn. When you are done, call %s exactly once with your answer. Cite " +
	"evidence only by the aliases (E1, E2, ...) shown with it."

const jsonRules = "Tools are called with JSON actions. Reply with exactly one JSON " +
	"object per turn and nothing else: {\"tool\": \"<name>\", \"args\": {...}}. You " +
	"may add \"plan\": \"<one line>\". When you are done reply with the %s action. " +
	"Cite evidence only by the aliases (E1, E2, ...) shown with it. Tools:\n"

func systemPrompt(cfg Config, p Protocol) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(cfg.System))
	b.WriteString("\n\n")
	if p == ProtocolJSON {
		fmt.Fprintf(&b, jsonRules, cfg.Final.Name)
		for _, t := range cfg.Tools {
			fmt.Fprintf(&b, "- %s: %s Args schema: %s\n", t.Name, t.Description,
				schemaText(t.Parameters))
		}
		fmt.Fprintf(&b, "- %s: %s Args schema: %s\n", cfg.Final.Name, cfg.Final.Description,
			schemaText(cfg.Final.Parameters))
	} else {
		fmt.Fprintf(&b, nativeRules, cfg.Final.Name)
	}
	b.WriteString("\n" + llm.UntrustedDataRule)
	return b.String()
}

func schemaText(raw []byte) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

// taskPrompt is the caller's task, the budget and the seeded evidence.
func taskPrompt(cfg Config) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(cfg.Task))
	fmt.Fprintf(&b, "\n\nBudget: %d model steps, %d tool calls, %d probe cost units.",
		cfg.Budget.MaxSteps, cfg.Budget.MaxCalls, cfg.Budget.MaxCost)
	if len(cfg.Seed) > 0 {
		var ev strings.Builder
		for i, e := range cfg.Seed {
			fmt.Fprintf(&ev, "E%d [%s] %s\n", i+1, e.Label, oneLine(e.Text))
		}
		b.WriteString("\nEvidence you may cite (by alias):\n")
		b.WriteString(llm.UntrustedData("evidence", ev.String()))
	}
	return b.String()
}

// firstMessages opens a conversation in protocol p.
func firstMessages(cfg Config, p Protocol) []llm.Message {
	return []llm.Message{{Role: "system", Content: systemPrompt(cfg, p)},
		{Role: "user", Content: taskPrompt(cfg)}}
}

// offered is the native tool list: every tool and the final, or only
// the final.
func offered(cfg Config, finalOnly bool) []llm.ToolSpec {
	final := llm.ToolSpec{Name: cfg.Final.Name, Description: cfg.Final.Description,
		Parameters: cfg.Final.Parameters}
	if finalOnly {
		return []llm.ToolSpec{final}
	}
	out := make([]llm.ToolSpec, 0, len(cfg.Tools)+1)
	for _, t := range cfg.Tools {
		out = append(out, llm.ToolSpec{Name: t.Name, Description: t.Description,
			Parameters: t.Parameters})
	}
	return append(out, final)
}

// EstimateTokens is what one call may cost before the provider says:
// about four bytes a token of the request as sent (message and tool
// framing, and an eighth more for JSON escaping of the text), plus the
// completion cap. It is meant to cover the LLM client's own admission
// estimate of the request body, so a caller may reserve exactly this.
func EstimateTokens(msgs []llm.Message, tools []llm.ToolSpec, stepTokens int) int {
	n := 256 // request envelope
	for _, m := range msgs {
		n += len(m.Content) + len(m.Content)/8 + 64
		for _, tc := range m.ToolCalls {
			n += len(tc.Name) + len(tc.Arguments) + len(tc.Arguments)/8 + 64
		}
	}
	for _, t := range tools {
		n += len(t.Name) + len(t.Description) + len(t.Parameters) + 96
	}
	return (n+3)/4 + stepTokens
}

// resultText is how one tool result reaches the model.
func resultText(tool string, out Output, alias string) string {
	status := out.Status
	if status == "" {
		status = "ok"
	}
	label := "result of " + tool
	head := fmt.Sprintf("[%s %s] (not citable)", tool, status)
	if alias != "" {
		label = "result " + alias
		head = fmt.Sprintf("%s [%s] (citable as %s)", alias, out.Evidence.Label, alias)
	}
	return head + "\n" + llm.UntrustedData(label, clip(out.Text, maxResultRunes))
}

func refusalText(tool, reason, detail string) string {
	return fmt.Sprintf("refused %s: %s: %s", tool, reason, clip(detail, maxNoteRunes))
}

func correction(cfg Config, reason, detail string, p Protocol) string {
	names := make([]string, 0, len(cfg.Tools)+1)
	for _, t := range cfg.Tools {
		names = append(names, t.Name)
	}
	names = append(names, cfg.Final.Name)
	how := "Call"
	if p == ProtocolJSON {
		how = "Reply with one JSON action for"
	}
	return fmt.Sprintf("Your last reply was refused (%s: %s). %s one of the listed "+
		"tools: %s.", reason, clip(detail, maxNoteRunes), how, strings.Join(names, ", "))
}

func lastStepNotice(cfg Config, p Protocol) string {
	if p == ProtocolJSON {
		return "No tool budget is left: reply now with the " + cfg.Final.Name +
			" action and your answer."
	}
	return "No tool budget is left: call " + cfg.Final.Name + " now with your answer."
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip keeps at most n runes of s.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}
