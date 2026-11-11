package sre

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre/runbook"
)

// Staging a draft beside the signed version (playbooks-10, owner question
// 4): an edit appends an unsigned draft, and the newest version whose
// signature is valid keeps running until an admin signs the draft. The
// draft never inherits a signature, a tampered signed version never runs,
// and retiring stops every version. Real PostgreSQL.

// tamperVersion rewrites one stored version behind the version guard, as
// a superuser could; its content hash then no longer matches.
func tamperVersion(t *testing.T, ctx context.Context, pool *pgxpool.Pool, scope Scope,
	id UUID, version int) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE sage.sre_runbook_versions
		SET definition = jsonb_set(definition, '{nodes,1,then}', '"escalate"')
		WHERE deployment_id = $1 AND database_id = $2 AND runbook_id = $3
		  AND version = $4`, string(scope.DeploymentID), string(scope.DatabaseID),
		string(id), version)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("tamper v%d: %v (%d rows)", version, err, tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func editedIdle(desc string) runbook.Definition {
	d := idleRunbook()
	d.Description = desc
	return d
}

func signVersion(t *testing.T, ctx context.Context, st *PostgresStore, scope Scope, id UUID,
	version int, d runbook.Definition) Runbook {
	t.Helper()
	rb, err := st.SignRunbook(ctx, scope, id, RunbookSignature{Version: version,
		ContentHash: hashOf(t, d), Signer: "user:1", Role: "admin"})
	if err != nil {
		t.Fatalf("sign v%d: %v", version, err)
	}
	return rb
}

// onlyRunnable returns the one runnable runbook, failing otherwise.
func onlyRunnable(t *testing.T, ctx context.Context, st *PostgresStore, scope Scope) Runbook {
	t.Helper()
	got, err := st.RunnableRunbooks(ctx, scope)
	if err != nil || len(got) != 1 {
		t.Fatalf("runnable = %+v, %v; want exactly one runbook", got, err)
	}
	return got[0]
}

func runningOf(rb Runbook) int {
	if rb.Running == nil {
		return 0
	}
	return rb.Running.Version
}

func TestRunbookStaging_SignedVersionKeepsRunningBesideADraft(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	rb := signedRunbook(t, ctx, st, scope, idleRunbook())
	v2, err := st.ReviseRunbook(ctx, scope, rb.ID, 1, draftOf(editedIdle("v2")))
	if err != nil || v2.LatestVersion != 2 || v2.Status != RunbookDraft || !v2.Runnable ||
		runningOf(v2) != 1 || v2.RunningVersion != 1 {
		t.Fatalf("revised = %+v, %v; want draft v2 latest with v1 still running", v2, err)
	}
	if v2.Latest.SignedAt != nil || v2.Latest.SignedBy != "" || v2.Latest.SignatureValid {
		t.Fatalf("draft v2 = %+v; a draft never inherits the signature", v2.Latest)
	}
	got := onlyRunnable(t, ctx, st, scope)
	if got.ID != rb.ID || runningOf(got) != 1 || got.LatestVersion != 2 ||
		got.Running.ContentHash != hashOf(t, idleRunbook()) || got.Running.SignedBy != "user:1" ||
		got.Running.Definition.Description != "" {
		t.Fatalf("runnable = %+v; want signed v1 to run, not the draft", got)
	}
	signed := signVersion(t, ctx, st, scope, rb.ID, 2, editedIdle("v2"))
	if signed.Status != RunbookSigned || runningOf(signed) != 2 || signed.RunningVersion != 2 {
		t.Fatalf("after signing v2 = %+v; want v2 running", signed)
	}
	got = onlyRunnable(t, ctx, st, scope)
	if runningOf(got) != 2 || got.Running.Definition.Description != "v2" ||
		got.Running.ContentHash != hashOf(t, editedIdle("v2")) {
		t.Fatalf("runnable after signing v2 = %+v; want v2", got.Running)
	}
}

func TestRunbookStaging_TamperedSignedVersionNeverRuns(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	rb := signedRunbook(t, ctx, st, scope, idleRunbook())
	if _, err := st.ReviseRunbook(ctx, scope, rb.ID, 1, draftOf(editedIdle("v2"))); err != nil {
		t.Fatal(err)
	}
	signVersion(t, ctx, st, scope, rb.ID, 2, editedIdle("v2"))
	tamperVersion(t, ctx, pool, scope, rb.ID, 2)
	got := onlyRunnable(t, ctx, st, scope)
	if runningOf(got) != 1 || got.Running.ContentHash != hashOf(t, idleRunbook()) ||
		!got.Running.SignatureValid {
		t.Fatalf("running = %+v; the tampered v2 must never run, v1 keeps running",
			got.Running)
	}
	full, err := st.GetRunbook(ctx, scope, rb.ID)
	if err != nil || full.Status != RunbookInvalid || full.Latest.SignatureValid ||
		runningOf(full) != 1 || !full.Runnable {
		t.Fatalf("get = %+v, %v; want status invalid (latest tampered), v1 running", full, err)
	}
}

func TestRunbookStaging_NoValidSignatureMeansNothingRuns(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	draft, err := st.CreateRunbook(ctx, scope, draftOf(idleRunbook()))
	if err != nil || draft.Runnable || draft.Running != nil || draft.RunningVersion != 0 {
		t.Fatalf("unsigned v1 = %+v, %v; want nothing running", draft, err)
	}
	// Boundary: the only signed version is tampered, and a draft stages on it.
	rb := signedRunbook(t, ctx, st, scope, editedIdle("other"))
	if _, err := st.ReviseRunbook(ctx, scope, rb.ID, 1, draftOf(editedIdle("v2"))); err != nil {
		t.Fatal(err)
	}
	tamperVersion(t, ctx, pool, scope, rb.ID, 1)
	if got, err := st.RunnableRunbooks(ctx, scope); err != nil || len(got) != 0 {
		t.Fatalf("runnable = %+v, %v; no version has a valid signature", got, err)
	}
	for _, id := range []UUID{draft.ID, rb.ID} {
		got, err := st.GetRunbook(ctx, scope, id)
		if err != nil || got.Runnable || got.Running != nil || got.Status != RunbookDraft {
			t.Fatalf("get %s = %+v, %v; want a draft that does not run", id, got, err)
		}
	}
	list, err := st.ListRunbooks(ctx, scope)
	if err != nil || len(list) != 2 || list[0].Runnable || list[1].Runnable {
		t.Fatalf("list = %+v, %v; neither runbook runs", list, err)
	}
}

func TestRunbookStaging_ListShowsTheRunningVersion(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	rb := signedRunbook(t, ctx, st, scope, idleRunbook())
	if _, err := st.ReviseRunbook(ctx, scope, rb.ID, 1, draftOf(editedIdle("v2"))); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListRunbooks(ctx, scope)
	if err != nil || len(list) != 1 || !list[0].Runnable || list[0].RunningVersion != 1 ||
		list[0].LatestVersion != 2 || list[0].Status != RunbookDraft {
		t.Fatalf("list = %+v, %v; want draft v2 with v1 running", list, err)
	}
}

func TestRunbookStaging_RetireStopsEveryVersion(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	rb := signedRunbook(t, ctx, st, scope, idleRunbook())
	if _, err := st.ReviseRunbook(ctx, scope, rb.ID, 1, draftOf(editedIdle("v2"))); err != nil {
		t.Fatal(err)
	}
	retired, err := st.RetireRunbook(ctx, scope, rb.ID, "user:1")
	if err != nil || retired.Runnable || retired.Running != nil ||
		retired.Status != RunbookRetired {
		t.Fatalf("retired = %+v, %v; the signed v1 must stop too", retired, err)
	}
	if got, _ := st.RunnableRunbooks(ctx, scope); len(got) != 0 {
		t.Fatalf("runnable after retiring = %+v", got)
	}
	if _, err := st.SignRunbook(ctx, scope, rb.ID, RunbookSignature{Version: 2,
		ContentHash: hashOf(t, editedIdle("v2")), Signer: "user:1",
		Role: "admin"}); !errors.Is(err, ErrRetired) {
		t.Fatalf("sign the retired draft = %v, want ErrRetired", err)
	}
}

func TestRunbookStaging_SigningRacingARevisionNeverRunsADraft(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	for round := 0; round < 5; round++ {
		rb := signedRunbook(t, ctx, st, scope, idleRunbook())
		if _, err := st.ReviseRunbook(ctx, scope, rb.ID, 1,
			draftOf(editedIdle("v2"))); err != nil {
			t.Fatal(err)
		}
		errs := race(2, func(i int) error {
			if i == 0 {
				_, err := st.SignRunbook(ctx, scope, rb.ID, RunbookSignature{Version: 2,
					ContentHash: hashOf(t, editedIdle("v2")), Signer: "user:1",
					Role: "admin"})
				return err
			}
			_, err := st.ReviseRunbook(ctx, scope, rb.ID, 2, draftOf(editedIdle("v3")))
			return err
		})
		got, err := st.GetRunbook(ctx, scope, rb.ID)
		want := 1
		if errs[0] == nil {
			want = 2
		}
		if err != nil || got.LatestVersion != 3 || runningOf(got) != want ||
			got.Latest.SignedAt != nil {
			t.Fatalf("round %d: %+v, %v (sign err %v); want draft v3 latest and v%d running",
				round, got, err, errs[0], want)
		}
		if strings.Contains(got.Running.Definition.Description, "v3") {
			t.Fatalf("round %d: the unsigned v3 runs", round)
		}
	}
}

func TestRunbookStaging_InvestigationRunsTheSignedVersionNotTheDraft(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	var rb Runbook
	inv, _ := runIdle(t, ctx, st, idleChainRunner(), func(scope Scope) {
		rb = signedRunbook(t, ctx, st, scope, idleRunbook())
		// The draft escalates where v1 ends the transaction; it must not run.
		d := editedIdle("v2")
		d.Nodes[1].Then, d.Nodes[1].Else = "escalate", "end_tx"
		if _, err := st.ReviseRunbook(ctx, scope, rb.ID, 1, draftOf(d)); err != nil {
			t.Fatal(err)
		}
	})
	run := inv.Summary.Runbook
	if run == nil || run.RunbookID != rb.ID || run.Version != 1 ||
		run.ContentHash != hashOf(t, idleRunbook()) || run.SignedBy != "user:1" ||
		strings.Join(run.Path, ",") != "read_chains,is_idle,end_tx" {
		t.Fatalf("summary runbook = %+v; want signed v1 to run beside the draft", run)
	}
	runs, err := st.RunbookRuns(ctx, inv.Scope, rb.ID, 10)
	if err != nil || len(runs) != 1 || runs[0].Version != 1 {
		t.Fatalf("run history = %+v, %v; want the v1 run recorded", runs, err)
	}
}
