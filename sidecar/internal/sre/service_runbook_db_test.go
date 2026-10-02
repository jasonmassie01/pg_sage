package sre

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/runbook"
)

// The runbook and memory surface of a database's investigator service:
// what the API and MCP tools call. Compiling English needs the model; a
// compiled draft keeps its (redacted) source text and the model that
// wrote it, and is never runnable until an admin signs it.

func idleRunbookJSON(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal(idleRunbook())
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func submitRunbook(def string) fakeReply {
	return callTool("submit_runbook", fixed(def))
}

func TestService_CompileStoresARedactedUnsignedDraft(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, submitRunbook(idleRunbookJSON(t)))
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), m.client())
	svc := NewService("orders", c, st)
	text := "When locks pile up read the lock chains; connect with " +
		"postgres://admin:s3cret@db:5432/app and end the idle transaction."
	rb, err := svc.CompileRunbook(ctx, text, "user:2")
	if err != nil {
		t.Fatalf("CompileRunbook: %v", err)
	}
	v := rb.Latest
	if rb.Status != RunbookDraft || rb.Runnable || v.Source != "compiled" ||
		v.CompiledBy != "m" || v.ContentHash != hashOf(t, idleRunbook()) ||
		rb.CreatedBy != "user:2" {
		t.Fatalf("compiled = %+v, want an unrunnable compiled draft", rb)
	}
	if strings.Contains(v.SourceText, "s3cret") || !strings.Contains(v.SourceText,
		"lock chains") {
		t.Fatalf("source text %q: want it kept and redacted", v.SourceText)
	}
	if strings.Contains(promptOf(t, m.body(t, 0)), "s3cret") {
		t.Fatal("the connection credential reached the model")
	}
}

func TestService_CompileRejectionStoresNothing(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	bad := strings.Replace(idleRunbookJSON(t), `"lock_chains"`, `"pg_kill_all"`, 1)
	m := newFakeModel(t, submitRunbook(bad), submitRunbook(bad))
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), m.client())
	svc := NewService("orders", c, st)
	_, err := svc.CompileRunbook(ctx, "Read the lock chains.", "user:2")
	var rej *runbook.Rejection
	if !errors.As(err, &rej) || rej.Reason != runbook.RejectInvalid ||
		rej.Problems[0].Code != runbook.CodeUnknownProbe {
		t.Fatalf("CompileRunbook = %v, want an invalid_definition rejection", err)
	}
	if list, _ := svc.Runbooks(ctx); len(list) != 0 {
		t.Fatalf("a rejected compile stored %+v", list)
	}
}

func TestService_CompileNeedsTheModel(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	c, _ := testCoordinator(t, ctx, st, idleChainRunner(), nil)
	svc := NewService("orders", c, st)
	if _, err := svc.CompileRunbook(ctx, "Read the locks.", "user:2"); !errors.Is(err,
		ErrModelUnavailable) {
		t.Fatalf("compile without a model = %v, want ErrModelUnavailable", err)
	}
	disabled := newFakeModel(t)
	c2, _ := modelCoordinator(t, ctx, st, idleChainRunner(), llmClient(disabled.srv.URL,
		false))
	if _, err := NewService("orders", c2, st).CompileRunbook(ctx, "Read the locks.",
		"user:2"); !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("compile with a disabled model = %v, want ErrModelUnavailable", err)
	}
	if _, err := NewService("orders", c, st).CompileRunbook(ctx, "", "user:2"); !errors.Is(
		err, ErrInvalidRequest) {
		t.Fatalf("compile of empty text = %v, want ErrInvalidRequest", err)
	}
}

func TestService_RunbookLifecycle(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	c, _ := testCoordinator(t, ctx, st, idleChainRunner(), nil)
	svc := NewService("orders", c, st)
	rb, err := svc.CreateRunbook(ctx, idleRunbook(), "user:2")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	hash := hashOf(t, idleRunbook())
	if _, err := svc.SignRunbook(ctx, rb.ID, 1, hash, "user:2", "operator"); !errors.Is(
		err, ErrInvalidRequest) {
		t.Fatalf("operator signature = %v, want ErrInvalidRequest", err)
	}
	signed, err := svc.SignRunbook(ctx, rb.ID, 1, hash, "user:1", "admin")
	if err != nil || !signed.Runnable {
		t.Fatalf("sign: %+v, %v", signed, err)
	}
	inv := startAndRun(t, ctx, c, lockTrigger("svc-rb"))
	runs, err := svc.RunbookRuns(ctx, rb.ID)
	if err != nil || len(runs) != 1 || runs[0].InvestigationID != inv.ID {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	edited := idleRunbook()
	edited.Description = "v2"
	v2, err := svc.ReviseRunbook(ctx, rb.ID, 1, edited, "user:2")
	if err != nil || v2.Runnable || v2.LatestVersion != 2 {
		t.Fatalf("revise: %+v, %v", v2, err)
	}
	if _, err := svc.RetireRunbook(ctx, rb.ID, "user:3"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	list, err := svc.Runbooks(ctx)
	if err != nil || len(list) != 1 || list[0].Status != RunbookRetired {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if got, err := svc.Runbook(ctx, rb.ID); err != nil || len(got.Versions) != 2 {
		t.Fatalf("get = %+v, %v", got, err)
	}
}

func TestService_SimilarAndOutcome(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	c, _ := testCoordinator(t, ctx, st, idleChainRunner(), nil)
	svc := NewService("orders", c, st)
	past := startAndRun(t, ctx, c, lockTrigger("svc-past"))
	if _, err := svc.RecordOutcome(ctx, past.ID, OutcomeRequest{Verdict: "confirmed",
		Actor: "user:4"}); err != nil {
		t.Fatalf("outcome: %v", err)
	}
	now := startAndRun(t, ctx, c, lockTrigger("svc-now"))
	got, err := svc.Similar(ctx, now.ID)
	if err != nil || len(got) != 1 || got[0].InvestigationID != past.ID ||
		got[0].Outcome == nil || got[0].Outcome.Verdict != OutcomeConfirmed {
		t.Fatalf("similar = %+v, %v", got, err)
	}
	if got, err := svc.Similar(ctx, past.ID); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("similar to the first = %+v, %v; want an empty, non-nil list", got, err)
	}
	if _, err := svc.Similar(ctx, NewUUID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("similar to an unknown id = %v", err)
	}
	var nilSvc *Service
	if _, err := nilSvc.Similar(ctx, now.ID); !errors.Is(err, ErrMetadataUnavailable) {
		t.Fatalf("nil service = %v", err)
	}
}
