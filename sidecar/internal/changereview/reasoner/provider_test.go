package reasoner

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/changereview"
)

// Provider trouble (CHECK-RSN-04) and budgets (CHECK-RSN-05): every
// failure is a model status with the transcript's stop reason, never an
// error, and the verdict stays the deterministic one.

func malformedToolCall() fakeReply {
	return func(w http.ResponseWriter, _ string) {
		writeCompletion(w, map[string]any{"role": "assistant", "content": "",
			"tool_calls": []map[string]any{{"id": "c1", "type": "function",
				"function": map[string]any{"name": ToolInspect, "arguments": "{not json"}}}},
			300)
	}
}

func TestReason_MalformedProviderBehavior(t *testing.T) {
	fenced := "```json\n{\"tool\":\"" + FinalTool + "\",\"args\":{\"findings\":[]}}\n```"
	slow := slowReply(1500*time.Millisecond, emptySubmit())
	cases := []struct {
		name   string
		script []fakeReply
		status string
		stop   string
	}{
		{"fenced final via JSON fallback", []fakeReply{statusReply(http.StatusBadRequest),
			contentReply(fenced)}, changereview.ModelOK, agentloop.StopFinal},
		{"fenced final in native content", []fakeReply{contentReply(fenced)},
			changereview.ModelOK, agentloop.StopFinal},
		{"empty replies", []fakeReply{contentReply(""), contentReply(""), contentReply("")},
			changereview.ModelError, agentloop.StopMalformed},
		{"malformed tool calls", []fakeReply{malformedToolCall(), malformedToolCall(),
			malformedToolCall()}, changereview.ModelError, agentloop.StopMalformed},
		{"429", []fakeReply{statusReply(http.StatusTooManyRequests)},
			changereview.ModelRateLimited, agentloop.StopRateLimited},
		{"step timeout", []fakeReply{slow}, changereview.ModelTimeout, agentloop.StopTimeout},
		{"500", nil, changereview.ModelError, agentloop.StopProvider},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := testBounds()
			b.StepTimeout = 400 * time.Millisecond
			r := newTestReasoner(t, Deps{Model: newFakeLLM(t, c.script...).client(), Bounds: b})
			res := reason(t, r, testInput(controllerDiff, ""), riskyAnalysis())
			if res.ModelStatus != c.status || res.Transcript == nil ||
				res.Transcript.Stop != c.stop {
				t.Fatalf("status %q stop %+v, want %s / %s", res.ModelStatus, res.Transcript,
					c.status, c.stop)
			}
			m, _ := rendered(riskyAnalysis(), res)
			if m.Verdict != changereview.VerdictRisky || len(modelFindings(m.Findings)) != 0 {
				t.Fatalf("verdict %s findings %+v, want the deterministic risky", m.Verdict,
					m.Findings)
			}
		})
	}
}

func TestReason_PerReviewTokenCapStopsTheLoop(t *testing.T) {
	fake := newFakeLLM(t, emptySubmit())
	in := testInput(controllerDiff, "")
	in.Config.MaxTokensPerReview = 4000
	res := reason(t, newTestReasoner(t, Deps{Model: fake.client()}), in, safeAnalysis())
	if res.ModelStatus != changereview.ModelBudget || res.Transcript == nil ||
		res.Transcript.Stop != agentloop.StopTokens {
		t.Fatalf("status %q transcript %+v, want budget / %s", res.ModelStatus, res.Transcript,
			agentloop.StopTokens)
	}
	if res.Tokens > 4000 {
		t.Fatalf("used %d tokens over the 4000 cap", res.Tokens)
	}
}

func TestReason_ExhaustedDailyBudgetMakesNoRequest(t *testing.T) {
	fake := newFakeLLM(t, emptySubmit())
	budget := &memBudget{limit: 1000, used: 1000}
	r := newTestReasoner(t, Deps{Model: fake.client(), Budget: budget})
	res := reason(t, r, testInput(controllerDiff, ""), safeAnalysis())
	if fake.calls() != 0 {
		t.Fatalf("%d requests reached the provider", fake.calls())
	}
	if res.ModelStatus != changereview.ModelBudget || res.Transcript.Stop != agentloop.StopBudget {
		t.Fatalf("status %q stop %q", res.ModelStatus, res.Transcript.Stop)
	}
	if budget.total() != 1000 || budget.reserves != 1 {
		t.Fatalf("budget used %d after %d reserves", budget.total(), budget.reserves)
	}
}

func TestReason_ZeroDailyBudgetSkipsTheModel(t *testing.T) {
	for _, mod := range []func(*changereview.Input){
		func(in *changereview.Input) { in.Config.DailyTokensPerDatabase = 0 },
		func(in *changereview.Input) { in.Config.DailyTokensPerActor = 0 },
	} {
		fake := newFakeLLM(t, emptySubmit())
		in := testInput(railsRenameDiff, "rails")
		mod(&in)
		res := reason(t, newTestReasoner(t, Deps{Model: fake.client()}), in, noSQLAnalysis())
		if fake.calls() != 0 || res.ModelStatus != changereview.ModelSkipped {
			t.Fatalf("status %q after %d requests, want skipped and none", res.ModelStatus,
				fake.calls())
		}
		if !hasRule(res.Findings, changereview.RuleNotAnalyzableSQL) {
			t.Fatalf("a skipped model on an ORM-only migration needs CR-20: %+v", res.Findings)
		}
	}
}

func TestReason_BudgetStoreFailureIsAModelError(t *testing.T) {
	fake := newFakeLLM(t, emptySubmit())
	r := newTestReasoner(t, Deps{Model: fake.client(), Budget: &memBudget{limit: 1 << 30,
		err: errStore}})
	res := reason(t, r, testInput(controllerDiff, ""), safeAnalysis())
	if fake.calls() != 0 || res.ModelStatus != changereview.ModelError {
		t.Fatalf("status %q after %d requests", res.ModelStatus, fake.calls())
	}
}

func TestReason_SettlesReportedUsage(t *testing.T) {
	fake := newFakeLLM(t, emptySubmit())
	budget := &memBudget{limit: 1 << 30}
	res := reason(t, newTestReasoner(t, Deps{Model: fake.client(), Budget: budget}),
		testInput(controllerDiff, ""), safeAnalysis())
	if res.ModelStatus != changereview.ModelOK || budget.total() != 300 ||
		len(budget.settled) != 1 || budget.settled[0] != 300 || res.Tokens != 300 {
		t.Fatalf("status %q, budget %d settled %v, tokens %d", res.ModelStatus, budget.total(),
			budget.settled, res.Tokens)
	}
}

func TestReason_ConcurrentReviewsShareTheBudget(t *testing.T) {
	budget := &memBudget{limit: 30000}
	var wg sync.WaitGroup
	results := make([]changereview.Reasoning, 6)
	for i := range results {
		fake := newFakeLLM(t, emptySubmit())
		r := newTestReasoner(t, Deps{Model: fake.client(), Budget: budget})
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			res, err := r.Reason(ctx, testInput(controllerDiff, ""), safeAnalysis())
			if err != nil {
				t.Errorf("Reason: %v", err)
			}
			results[i] = res
		}()
	}
	wg.Wait()
	if budget.total() > 30000 {
		t.Fatalf("budget at %d over its 30000 cap", budget.total())
	}
	ok := 0
	for _, res := range results {
		switch res.ModelStatus {
		case changereview.ModelOK:
			ok++
		case changereview.ModelBudget:
		default:
			t.Fatalf("status %q, want ok or budget", res.ModelStatus)
		}
	}
	if ok == 0 {
		t.Fatal("no review got the model")
	}
}

func TestReason_CancelledContextIsAnError(t *testing.T) {
	r := newTestReasoner(t, Deps{Model: newFakeLLM(t, emptySubmit()).client()})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Reason(ctx, testInput(controllerDiff, ""), safeAnalysis()); !errors.Is(err,
		context.Canceled) {
		t.Fatalf("Reason on a cancelled context: %v, want context.Canceled", err)
	}
}

func TestReason_WallClockEndsBeforeTheCallerDeadline(t *testing.T) {
	slow := slowReply(3*time.Second, emptySubmit())
	r := newTestReasoner(t, Deps{Model: newFakeLLM(t, slow, slow).client()})
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := r.Reason(ctx, testInput(controllerDiff, ""), safeAnalysis())
	if err != nil {
		t.Fatalf("Reason: %v", err)
	}
	if time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("the run took %v, past the caller's deadline", time.Since(start))
	}
	if res.ModelStatus != changereview.ModelTimeout {
		t.Fatalf("status %q, want timeout", res.ModelStatus)
	}
}

func TestModelStatusOfEveryStop(t *testing.T) {
	want := map[string]string{
		agentloop.StopFinal: changereview.ModelOK, agentloop.StopTokens: changereview.ModelBudget,
		agentloop.StopBudget:      changereview.ModelBudget,
		agentloop.StopRateLimited: changereview.ModelRateLimited,
		agentloop.StopCooldown:    changereview.ModelRateLimited,
		agentloop.StopTimeout:     changereview.ModelTimeout,
		agentloop.StopWall:        changereview.ModelTimeout,
		agentloop.StopDisabled:    changereview.ModelNoModel,
		agentloop.StopProvider:    changereview.ModelError,
		agentloop.StopMalformed:   changereview.ModelError,
		agentloop.StopMaxSteps:    changereview.ModelError,
		"":                        changereview.ModelError, "something new": changereview.ModelError,
	}
	for stop, status := range want {
		if got := modelStatus(stop); got != status {
			t.Errorf("modelStatus(%q) = %q, want %q", stop, got, status)
		}
	}
}

func TestReason_TranscriptIsRedacted(t *testing.T) {
	leaky := "SELECT 1 FROM orders /* postgres://app:s3cret@db/prod */"
	fake := newFakeLLM(t,
		calls(toolCall{ToolInspect, jsonArgs(map[string]any{"sql": leaky,
			"origin": "extracted"})}),
		emptySubmit())
	res := reason(t, newTestReasoner(t, Deps{Model: fake.client()}),
		testInput(controllerDiff, ""), safeAnalysis())
	raw, err := jsonOf(res.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "s3cret") || !strings.Contains(raw, "[redacted]") {
		t.Fatalf("transcript not redacted: %s", raw)
	}
}
