package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/llm"
)

// ActionJustifier produces a plain-English justification for an
// autonomous action. llm.Client satisfies it directly via Chat. nil
// disables justification (C4).
type ActionJustifier interface {
	Chat(ctx context.Context, system, user string, maxTokens int) (string, int, error)
}

// WithJustifier attaches an LLM client used to write a human-readable
// justification into the action log for each executed action (C4) — the
// accountability record that makes autonomous DDL auditable.
func (e *Executor) WithJustifier(j ActionJustifier) { e.justifier = j }

const justifySystemPrompt = `You are a PostgreSQL DBA writing a concise audit ` +
	`note. In 2-3 plain-English sentences, explain to an operator: why this ` +
	`maintenance action was taken, its expected effect, and how it is reversed ` +
	`if needed. Be specific and factual. Do not use markdown. Output only the note. ` +
	llm.UntrustedDataRule

// buildJustificationPrompt builds the user prompt for a finding's action.
// Object names, titles and reasons carry database or model-written text,
// so every field but the internal category is delimited as untrusted data;
// SQL is also comment-stripped and literal-redacted. Pure and testable.
func buildJustificationPrompt(f analyzer.Finding) string {
	rollback := f.RollbackSQL
	if rollback == "" {
		rollback = "(no rollback needed; action is idempotent/safe maintenance)"
	}
	return fmt.Sprintf(
		"Action category: %s\nObject:\n%s\nFinding:\n%s\nReason:\n%s\n"+
			"Executed SQL:\n%s\nRollback SQL:\n%s",
		f.Category,
		llm.UntrustedData("object", f.ObjectIdentifier),
		llm.UntrustedData("finding", f.Title),
		llm.UntrustedData("reason", f.Recommendation),
		llm.SanitizePromptSQL("executed_sql", f.RecommendedSQL),
		llm.SanitizePromptSQL("rollback_sql", rollback))
}

// justifyAndStore generates a justification for an executed action and
// records it on the action_log row. Runs best-effort: failures are
// logged and never block or fail the action.
func (e *Executor) justifyAndStore(
	ctx context.Context, actionID int64, f analyzer.Finding,
) {
	if e.justifier == nil || actionID <= 0 {
		return
	}
	jctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	note, _, err := e.justifier.Chat(
		jctx, justifySystemPrompt, buildJustificationPrompt(f), 256)
	if err != nil {
		e.logFn("executor", "justification failed for action %d: %v",
			actionID, err)
		return
	}
	// json_mode wraps prose in a JSON object; store plain text (C4).
	note = llm.UnwrapText(note)
	if _, err := e.pool.Exec(jctx,
		`/* pg_sage */ UPDATE sage.action_log SET justification = $1 WHERE id = $2`,
		note, actionID,
	); err != nil {
		e.logFn("executor", "store justification for action %d: %v",
			actionID, err)
	}
}
