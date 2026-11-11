package briefing

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/selfbudget"
)

// Result is one generated briefing: its stored row (ID 0 when the store
// failed), the text to dispatch and the payload stored as content_json.
type Result struct {
	ID      int64
	Text    string
	Payload Payload
}

// Generate creates and stores a briefing and returns its text.
func (w *Worker) Generate(ctx context.Context) (string, error) {
	r, err := w.GenerateBriefing(ctx)
	return r.Text, err
}

// GenerateBriefing creates a briefing over the window since the previous
// one (llm-06), stores it, and returns it.
func (w *Worker) GenerateBriefing(ctx context.Context) (Result, error) {
	ctx, done := selfbudget.Process().TrackFor(ctx, "briefing")
	defer done()
	w.lastGenerated = time.Time{}
	// sage.briefings keeps microseconds; the window must round-trip exactly.
	w.window = w.windowFor(time.Now().Truncate(time.Microsecond))
	findings, totalOpen, err := w.gatherFindings(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("gather findings: %w", err)
	}
	system, err := w.gatherSystem(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("gather system: %w", err)
	}
	actions, err := w.gatherRecentActions(ctx, w.window)
	if err != nil {
		w.logFn("WARN", "briefing: gather recent actions: %v", err)
		actions = "[]" // non-fatal
	}
	payload, err := buildPayload(findings, totalOpen, system, actions, w.database)
	if err != nil {
		return Result{}, fmt.Errorf("build briefing payload: %w", err)
	}
	payload.Window = w.window
	if payload.OpenBySeverity, err = w.gatherSeverityTotals(ctx); err != nil {
		w.logFn("WARN", "briefing: count open findings by severity: %v", err)
	}
	structured := w.buildStructured(findings, totalOpen, system, actions)

	// If LLM enabled, rewrite in natural language, grounded in the payload.
	if w.llm != nil && w.llm.IsEnabled() && !w.llm.IsCircuitOpen() {
		stored, plain, tokens, err := w.briefWithLLM(ctx, payload)
		if err != nil {
			w.logFn("WARN", "briefing: LLM enhancement failed: %v, using structured", err)
		} else {
			id := w.storeBriefing(ctx, stored, true, tokens, payload)
			return Result{ID: id, Text: plain, Payload: payload}, nil
		}
	}
	id := w.storeBriefing(ctx, structured, false, 0, payload)
	return Result{ID: id, Text: structured, Payload: payload}, nil
}

// storeBriefing records the briefing with its payload as content_json and
// its window as period_start/period_end, and returns the row id (0 on
// failure, which is logged).
func (w *Worker) storeBriefing(
	ctx context.Context, content string, llmUsed bool, tokens int, payload Payload,
) int64 {
	contentJSON, err := json.Marshal(payload)
	if err != nil {
		w.logFn("WARN", "briefing: encode content_json: %v; storing {}", err)
		contentJSON = []byte(`{}`)
	}
	win := payload.Window
	var id int64
	err = w.pool.QueryRow(ctx, `/* pg_sage */
		INSERT INTO sage.briefings (generated_at, period_start, period_end, mode,
			content_text, content_json, llm_used, token_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id
	`, win.End, win.Start, win.End, "executive", content,
		json.RawMessage(contentJSON), llmUsed, tokens).Scan(&id)
	if err != nil {
		w.logFn("WARN", "briefing: failed to store briefing: %v", err)
		return 0
	}
	w.lastGenerated = win.End
	return id
}

// Run generates, stores and dispatches one briefing, records each channel's
// outcome on the stored row, and marks the worker as run. A failed delivery
// is recorded, not retried, so a broken webhook never re-sends every cycle.
func (w *Worker) Run(ctx context.Context) error {
	r, err := w.GenerateBriefing(ctx)
	if err != nil {
		return err
	}
	status := w.Dispatch(ctx, r.Text)
	w.recordDelivery(ctx, r.ID, status)
	w.MarkRan()
	return nil
}

// recordDelivery stores status on row id, logging a failure.
func (w *Worker) recordDelivery(ctx context.Context, id int64, status map[string]Delivery) {
	if id <= 0 {
		return // the store already logged its failure
	}
	if err := w.RecordDelivery(ctx, id, status); err != nil {
		w.logFn("WARN", "briefing: %v", err)
	}
}
