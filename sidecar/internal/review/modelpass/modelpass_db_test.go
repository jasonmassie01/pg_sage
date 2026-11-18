package modelpass

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/review/report"
)

// review-03a on a real server: a full pass against an OpenAI-compatible
// httptest provider writes one sage.review_model_run row per run with its
// hashes, and with the model disabled the review still finishes with its
// grade and templated report (CHECK-26, CHECK-27).

// toolServer answers every chat completion with a submit_review call.
func toolServer(t *testing.T, a answer) *httptest.Server {
	t.Helper()
	args, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"finish_reason": "tool_calls",
				"message": map[string]any{"role": "assistant", "content": "",
					"tool_calls": []map[string]any{{"id": "call_1", "type": "function",
						"function": map[string]any{"name": finalName,
							"arguments": string(args)}}}}}},
			"usage": map[string]int{"prompt_tokens": 1200, "completion_tokens": 300,
				"total_tokens": 1500}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestModelPassDB_FullPassWritesHashedRuns(t *testing.T) {
	pool, ctx := livePool(t)
	in := input(t, "orders")
	in.RunID = newRun(t, ctx, pool, "orders")
	srv := toolServer(t, keepRanked(t, in, "t_a", "t_b"))
	inPrice, outPrice := 2.0, 8.0
	d := deps(realClient(srv.URL, 5), NewStore(pool))
	d.ModelName = "fake-model"
	d.Price = llm.PricingFrom(map[string]config.LLMPrice{"fake-model": {
		InputPerM: &inPrice, OutputPerM: &outPrice}})
	rep, err := Review(ctx, settings(3, "majority"), d, in)
	if err != nil || rep.ModelPass != report.PassDone || ids(rep.Ranked) != "t_a,t_b" {
		t.Fatalf("pass %s %q ranked %s (%v)", rep.ModelPass, rep.ModelPassReason,
			ids(rep.Ranked), err)
	}
	runs, err := NewStore(pool).ListRuns(ctx, in.RunID)
	if err != nil || len(runs) != 3 {
		t.Fatalf("stored %d runs (%v), want 3", len(runs), err)
	}
	for i, r := range runs {
		sum := sha256.Sum256([]byte(r.RawOutput))
		if r.Attempt != i+1 || r.Status != StatusDone || r.PackHash != in.Pack.ContentSHA256 ||
			r.OutputSHA256 != hex.EncodeToString(sum[:]) || r.TokensIn != 1200 ||
			r.TokensOut != 300 || r.CostUSD == nil || *r.CostUSD != 0.0048 ||
			r.KeptClaims != 2 || r.Model != "fake-model" || r.PromptVersion != PromptVersion {
			t.Errorf("run %d = %+v", i+1, r)
		}
	}
	_, err = pool.Exec(ctx, `UPDATE sage.review_model_run SET status = 'failed'`)
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("a stored run was updated: %v", err)
	}
}

func TestModelPassDB_ModelDisabledStillGradesAndReports(t *testing.T) {
	pool, ctx := livePool(t)
	in := input(t, "orders")
	in.RunID = newRun(t, ctx, pool, "orders")
	off := llm.New(&config.LLMConfig{Enabled: false, Model: "m"},
		func(string, string, ...any) {})
	rep, err := Review(ctx, settings(3, "majority"), deps(off, NewStore(pool)), in)
	if err != nil || rep.ModelPass != report.PassSkipped ||
		!strings.Contains(rep.ModelPassReason, "disabled") {
		t.Fatalf("pass %s %q (%v), want skipped: the model is disabled", rep.ModelPass,
			rep.ModelPassReason, err)
	}
	checkFallback(t, rep, in)
	runs, err := NewStore(pool).ListRuns(ctx, in.RunID)
	if err != nil || len(runs) != 0 {
		t.Fatalf("stored %d runs (%v) for a pass that never ran", len(runs), err)
	}
}

func TestModelPassDB_RecordRunRoundTripsAndRefusesBadRows(t *testing.T) {
	pool, ctx := livePool(t)
	run := newRun(t, ctx, pool, "orders")
	s := NewStore(pool)
	hash := strings.Repeat("a", 64)
	r := RunRecord{RunID: run, Attempt: 1, Role: RoleRank, Model: "m", PromptVersion: "v",
		PackHash: hash, RawOutput: "x", OutputSHA256: hash, Status: StatusFailed,
		ErrorClass: "rate", Reason: "the run stopped: rate_limited"}
	if err := s.RecordRun(ctx, r); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, err := s.ListRuns(ctx, run)
	if err != nil || len(got) != 1 || got[0].ErrorClass != "rate" || got[0].CostUSD != nil ||
		got[0].Reason != r.Reason || len(got[0].DroppedClaims) != 0 {
		t.Fatalf("round trip = %+v (%v)", got, err)
	}
	if err := s.RecordRun(ctx, r); err == nil || !strings.Contains(err.Error(), "model run 1") {
		t.Fatalf("a second row for attempt 1: %v, want a refusal naming it", err)
	}
	r.Attempt, r.Status = 2, "running"
	if err := s.RecordRun(ctx, r); err == nil {
		t.Fatal("a non-final status was stored")
	}
}
