package sre

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/runbook"
)

// Durable, versioned runbooks (AI-SRE-SPEC §7.1, §10 sre_runbooks)
// against real PostgreSQL. A version's content never changes; editing
// appends a draft version; a signature binds one version's exact content
// hash; only the latest version, signed, unretired and still matching its
// hash, is runnable. Every row is scoped to one database.

func TestRunbookStore_CreateStoresAnUnsignedDraft(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	rb, err := st.CreateRunbook(ctx, scope, draftOf(idleRunbook()))
	if err != nil {
		t.Fatalf("CreateRunbook: %v", err)
	}
	if rb.LatestVersion != 1 || rb.Status != RunbookDraft || rb.Runnable ||
		rb.Latest.ContentHash != hashOf(t, idleRunbook()) || rb.Latest.Source != "manual" ||
		rb.Latest.SignedAt != nil || rb.CreatedBy != "user:2" || rb.Latest.Name != "Idle holder" {
		t.Fatalf("created = %+v, want an unsigned manual draft v1", rb)
	}
	got, err := st.GetRunbook(ctx, scope, rb.ID)
	if err != nil || len(got.Versions) != 1 || got.Versions[0].Definition.Start != "read_chains" {
		t.Fatalf("GetRunbook = %+v, %v", got, err)
	}
	if runnable, err := st.RunnableRunbooks(ctx, scope); err != nil || len(runnable) != 0 {
		t.Fatalf("runnable = %v, %v; a draft must never be runnable", runnable, err)
	}
}

func TestRunbookStore_RefusesInvalidDefinitions(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	bad := idleRunbook()
	bad.Nodes[0].Probe = "pg_sleep"
	_, err := st.CreateRunbook(ctx, scope, draftOf(bad))
	var problems runbook.Problems
	if !errors.Is(err, ErrInvalidRequest) || !errors.As(err, &problems) ||
		problems[0].Code != runbook.CodeUnknownProbe {
		t.Fatalf("CreateRunbook(invalid) = %v, want ErrInvalidRequest with problems", err)
	}
	noActor := draftOf(idleRunbook())
	noActor.Actor = ""
	if _, err := st.CreateRunbook(ctx, scope, noActor); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("CreateRunbook(no actor) = %v, want ErrInvalidRequest", err)
	}
	if list, err := st.ListRunbooks(ctx, scope); err != nil || len(list) != 0 {
		t.Fatalf("list after refusals = %v, %v; want nothing stored", list, err)
	}
	if _, err := st.CreateRunbook(ctx, Scope{}, draftOf(idleRunbook())); !errors.Is(err,
		ErrInvalidRequest) {
		t.Fatalf("CreateRunbook(empty scope) = %v", err)
	}
}

func TestRunbookStore_SignBindsTheExactHash(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	rb, _ := st.CreateRunbook(ctx, scope, draftOf(idleRunbook()))
	hash := hashOf(t, idleRunbook())
	sig := func(version int, h, role string) RunbookSignature {
		return RunbookSignature{Version: version, ContentHash: h, Signer: "user:1", Role: role}
	}
	wrong := strings.Repeat("0", 64)
	if _, err := st.SignRunbook(ctx, scope, rb.ID, sig(1, wrong, "admin")); !errors.Is(err,
		ErrHashMismatch) {
		t.Fatalf("sign with another hash = %v, want ErrHashMismatch", err)
	}
	if _, err := st.SignRunbook(ctx, scope, rb.ID, sig(1, hash, "operator")); !errors.Is(err,
		ErrInvalidRequest) {
		t.Fatalf("operator signature = %v, want ErrInvalidRequest", err)
	}
	if _, err := st.SignRunbook(ctx, scope, rb.ID, sig(2, hash, "admin")); !errors.Is(err,
		ErrVersionConflict) {
		t.Fatalf("sign a version that does not exist = %v, want ErrVersionConflict", err)
	}
	signed, err := st.SignRunbook(ctx, scope, rb.ID, sig(1, strings.ToUpper(hash), "admin"))
	if err != nil || signed.Status != RunbookSigned || !signed.Runnable ||
		signed.Latest.SignedBy != "user:1" || signed.Latest.SignedAt == nil ||
		!signed.Latest.SignatureValid {
		t.Fatalf("signed = %+v, %v; want a runnable signed v1", signed, err)
	}
	if _, err := st.SignRunbook(ctx, scope, rb.ID, sig(1, hash, "admin")); !errors.Is(err,
		ErrAlreadySigned) {
		t.Fatalf("second signature = %v, want ErrAlreadySigned", err)
	}
	runnable, err := st.RunnableRunbooks(ctx, scope)
	if err != nil || len(runnable) != 1 || runnable[0].ID != rb.ID ||
		runnable[0].Latest.Version != 1 {
		t.Fatalf("runnable = %+v, %v; want the signed v1", runnable, err)
	}
}

func TestRunbookStore_EditingInvalidatesTheSignature(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	rb := signedRunbook(t, ctx, st, scope, idleRunbook())
	edited := idleRunbook()
	edited.Nodes[1].Else = "end_tx"
	edited.Nodes = edited.Nodes[:3]
	if _, err := st.ReviseRunbook(ctx, scope, rb.ID, 0, draftOf(edited)); !errors.Is(err,
		ErrVersionConflict) {
		t.Fatalf("revise on a stale base = %v, want ErrVersionConflict", err)
	}
	v2, err := st.ReviseRunbook(ctx, scope, rb.ID, 1, draftOf(edited))
	if err != nil || v2.LatestVersion != 2 || v2.Status != RunbookDraft || v2.Runnable ||
		v2.Latest.ContentHash != hashOf(t, edited) {
		t.Fatalf("revised = %+v, %v; want an unsigned, unrunnable v2", v2, err)
	}
	if runnable, _ := st.RunnableRunbooks(ctx, scope); len(runnable) != 0 {
		t.Fatalf("runnable after the edit = %+v; the signed v1 must not keep running",
			runnable)
	}
	got, _ := st.GetRunbook(ctx, scope, rb.ID)
	if len(got.Versions) != 2 || got.Versions[0].Version != 2 ||
		got.Versions[1].SignedAt == nil || !got.Versions[1].SignatureValid {
		t.Fatalf("versions = %+v; want v2 draft first and v1 still signed", got.Versions)
	}
	if _, err := st.SignRunbook(ctx, scope, rb.ID, RunbookSignature{Version: 1,
		ContentHash: hashOf(t, idleRunbook()), Signer: "user:1",
		Role: "admin"}); !errors.Is(err, ErrVersionConflict) && !errors.Is(err,
		ErrAlreadySigned) {
		t.Fatalf("re-sign of the superseded v1 = %v", err)
	}
	if _, err := st.ReviseRunbook(ctx, scope, rb.ID, 2, draftOf(edited)); err != nil {
		t.Fatalf("revise with the same content: %v", err)
	}
}

func TestRunbookStore_VersionsAreImmutable(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	rb := signedRunbook(t, ctx, st, scope, idleRunbook())
	for _, stmt := range []string{
		`UPDATE sage.sre_runbook_versions SET name = 'other'
		 WHERE deployment_id = $1 AND database_id = $2 AND runbook_id = $3`,
		`UPDATE sage.sre_runbook_versions SET definition = definition || '{"start":"x"}'
		 WHERE deployment_id = $1 AND database_id = $2 AND runbook_id = $3`,
		`UPDATE sage.sre_runbook_versions SET signed_by = 'user:9'
		 WHERE deployment_id = $1 AND database_id = $2 AND runbook_id = $3`,
		`DELETE FROM sage.sre_runbook_versions
		 WHERE deployment_id = $1 AND database_id = $2 AND runbook_id = $3`,
	} {
		_, err := pool.Exec(ctx, stmt, string(scope.DeploymentID), string(scope.DatabaseID),
			string(rb.ID))
		if err == nil || !strings.Contains(err.Error(), "runbook") {
			t.Errorf("%s: err = %v, want the version guard to refuse it", stmt, err)
		}
	}
	got, err := st.GetRunbook(ctx, scope, rb.ID)
	if err != nil || got.Latest.Name != "Idle holder" || !got.Runnable {
		t.Fatalf("after refused writes: %+v, %v", got, err)
	}
}

func TestRunbookStore_TamperedContentIsNeverRunnable(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	rb := signedRunbook(t, ctx, st, scope, idleRunbook())
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A superuser bypasses the guard; the content hash still exposes it.
	for _, stmt := range []string{`SET LOCAL session_replication_role = replica`,
		`UPDATE sage.sre_runbook_versions
		 SET definition = jsonb_set(definition, '{nodes,1,then}', '"escalate"')
		 WHERE deployment_id = $1 AND database_id = $2 AND runbook_id = $3`} {
		args := []any{string(scope.DeploymentID), string(scope.DatabaseID), string(rb.ID)}
		if !strings.Contains(stmt, "$1") {
			args = nil
		}
		if _, err := tx.Exec(ctx, stmt, args...); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("tamper: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if runnable, err := st.RunnableRunbooks(ctx, scope); err != nil || len(runnable) != 0 {
		t.Fatalf("runnable = %+v, %v; tampered content must never run", runnable, err)
	}
	got, err := st.GetRunbook(ctx, scope, rb.ID)
	if err != nil || got.Runnable || got.Status != RunbookInvalid ||
		got.Latest.SignatureValid {
		t.Fatalf("tampered runbook = %+v, %v; want status invalid", got, err)
	}
}

func TestRunbookStore_RetireStopsItForGood(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	rb := signedRunbook(t, ctx, st, scope, idleRunbook())
	retired, err := st.RetireRunbook(ctx, scope, rb.ID, "user:3")
	if err != nil || retired.Status != RunbookRetired || retired.Runnable ||
		retired.RetiredBy != "user:3" || retired.RetiredAt == nil {
		t.Fatalf("retired = %+v, %v", retired, err)
	}
	if runnable, _ := st.RunnableRunbooks(ctx, scope); len(runnable) != 0 {
		t.Fatalf("runnable after retiring = %+v", runnable)
	}
	if _, err := st.ReviseRunbook(ctx, scope, rb.ID, 1, draftOf(idleRunbook())); !errors.Is(
		err, ErrRetired) {
		t.Fatalf("revise retired = %v, want ErrRetired", err)
	}
	if _, err := st.RetireRunbook(ctx, scope, rb.ID, "user:3"); !errors.Is(err, ErrRetired) {
		t.Fatalf("retire twice = %v, want ErrRetired", err)
	}
	rb2, _ := st.CreateRunbook(ctx, scope, draftOf(idleRunbook()))
	if _, err := st.RetireRunbook(ctx, scope, rb2.ID, "user:3"); err != nil {
		t.Fatalf("retire a draft: %v", err)
	}
	if _, err := st.SignRunbook(ctx, scope, rb2.ID, RunbookSignature{Version: 1,
		ContentHash: hashOf(t, idleRunbook()), Signer: "user:1",
		Role: "admin"}); !errors.Is(err, ErrRetired) {
		t.Fatalf("sign retired = %v, want ErrRetired", err)
	}
}

func TestRunbookStore_ScopedToOneDatabase(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	a, b := testScope(t, ctx, st), testScope(t, ctx, st)
	rb := signedRunbook(t, ctx, st, a, idleRunbook())
	if _, err := st.GetRunbook(ctx, b, rb.ID); !errors.Is(err, ErrRunbookNotFound) ||
		!errors.Is(err, ErrNotFound) {
		t.Fatalf("get from another database = %v, want ErrRunbookNotFound", err)
	}
	for name, err := range map[string]error{
		"sign": func() error {
			_, err := st.SignRunbook(ctx, b, rb.ID, RunbookSignature{Version: 1,
				ContentHash: hashOf(t, idleRunbook()), Signer: "user:1", Role: "admin"})
			return err
		}(),
		"revise": func() error {
			_, err := st.ReviseRunbook(ctx, b, rb.ID, 1, draftOf(idleRunbook()))
			return err
		}(),
		"retire": func() error { _, err := st.RetireRunbook(ctx, b, rb.ID, "u"); return err }(),
	} {
		if !errors.Is(err, ErrRunbookNotFound) {
			t.Errorf("%s from another database = %v, want ErrRunbookNotFound", name, err)
		}
	}
	if list, _ := st.ListRunbooks(ctx, b); len(list) != 0 {
		t.Fatalf("other database lists %+v", list)
	}
	if runnable, _ := st.RunnableRunbooks(ctx, b); len(runnable) != 0 {
		t.Fatalf("other database runs %+v", runnable)
	}
	if _, err := st.GetRunbook(ctx, a, NewUUID()); !errors.Is(err, ErrRunbookNotFound) {
		t.Fatalf("unknown id = %v", err)
	}
	if _, err := st.GetRunbook(ctx, a, "not-a-uuid"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("malformed id = %v", err)
	}
}

func TestRunbookStore_ConcurrentSignaturesSignOnce(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	rb, _ := st.CreateRunbook(ctx, scope, draftOf(idleRunbook()))
	errs := race(8, func(i int) error {
		_, err := st.SignRunbook(ctx, scope, rb.ID, RunbookSignature{Version: 1,
			ContentHash: hashOf(t, idleRunbook()), Signer: "user:" + itoa(int64(i)),
			Role: "admin"})
		return err
	})
	if n := count(errs, nil); n != 1 {
		t.Fatalf("%d signatures succeeded, want exactly 1: %v", n, errs)
	}
	for _, err := range errs {
		if err != nil && !errors.Is(err, ErrAlreadySigned) {
			t.Fatalf("losing signer got %v, want ErrAlreadySigned", err)
		}
	}
}

func TestRunbookStore_ConcurrentEditsOnOneBase(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	rb, _ := st.CreateRunbook(ctx, scope, draftOf(idleRunbook()))
	errs := race(6, func(i int) error {
		d := idleRunbook()
		d.Description = "edit " + itoa(int64(i))
		_, err := st.ReviseRunbook(ctx, scope, rb.ID, 1, draftOf(d))
		return err
	})
	if n := count(errs, nil); n != 1 {
		t.Fatalf("%d edits on base 1 succeeded, want exactly 1: %v", n, errs)
	}
	got, _ := st.GetRunbook(ctx, scope, rb.ID)
	if got.LatestVersion != 2 || len(got.Versions) != 2 {
		t.Fatalf("after racing edits: latest %d with %d versions, want 2 and 2",
			got.LatestVersion, len(got.Versions))
	}
}

func TestRunbookStore_SignRacingAnEditNeverLeavesAStaleRunnable(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	for round := 0; round < 5; round++ {
		rb, _ := st.CreateRunbook(ctx, scope, draftOf(idleRunbook()))
		edited := idleRunbook()
		edited.Description = "edited"
		race(2, func(i int) error {
			if i == 0 {
				_, err := st.SignRunbook(ctx, scope, rb.ID, RunbookSignature{Version: 1,
					ContentHash: hashOf(t, idleRunbook()), Signer: "user:1", Role: "admin"})
				return err
			}
			_, err := st.ReviseRunbook(ctx, scope, rb.ID, 1, draftOf(edited))
			return err
		})
		got, err := st.GetRunbook(ctx, scope, rb.ID)
		if err != nil || got.LatestVersion != 2 || got.Runnable {
			t.Fatalf("round %d: %+v, %v; the edited draft must be latest and unrunnable",
				round, got, err)
		}
	}
}

func race(n int, fn func(i int) error) []error {
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
	return errs
}

func count(errs []error, target error) int {
	n := 0
	for _, err := range errs {
		if err == target {
			n++
		}
	}
	return n
}

func TestRunbookStore_RunHistory(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	lease := claimed(t, st, "pid 77")
	rb := signedRunbook(t, ctx, st, lease.Scope, idleRunbook())
	run := RunbookRun{Label: RunbookRunLabel, RunbookID: rb.ID, Version: 1,
		Name: "Idle holder", ContentHash: hashOf(t, idleRunbook()), SignedBy: "user:1",
		Outcome: RunbookCompleted, Path: []string{"read_chains", "is_idle", "end_tx"},
		Probes: 1, Proposal: &RunbookProposal{Label: RunbookProposalLabel,
			Kind: "operator_step", Node: "idle_in_tx_holder", Text: "End it."}}
	for i := 0; i < 2; i++ {
		if err := st.RecordRunbookRun(ctx, lease, run); err != nil {
			t.Fatalf("record run %d: %v", i, err)
		}
	}
	runs, err := st.RunbookRuns(ctx, lease.Scope, rb.ID, 10)
	if err != nil || len(runs) != 1 || runs[0].InvestigationID != lease.InvestigationID ||
		runs[0].Outcome != RunbookCompleted || len(runs[0].Path) != 3 ||
		runs[0].Proposal == nil || runs[0].Proposal.Node != "idle_in_tx_holder" ||
		runs[0].CreatedAt.IsZero() {
		t.Fatalf("runs = %+v, %v; want the one idempotent run", runs, err)
	}
	other := testScope(t, ctx, st)
	if runs, _ := st.RunbookRuns(ctx, other, rb.ID, 10); len(runs) != 0 {
		t.Fatalf("other database sees runs %+v", runs)
	}
	bad := run
	bad.Outcome = "exploded"
	if err := st.RecordRunbookRun(ctx, lease, bad); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("record unknown outcome = %v, want ErrInvalidRequest", err)
	}
}

func TestRunbookStore_UnavailableStoreIsDistinguishable(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	scope := testScope(t, ctx, st)
	pool.Close()
	_, err := st.ListRunbooks(ctx, scope)
	if !errors.Is(err, ErrMetadataUnavailable) {
		t.Fatalf("list on a closed pool = %v, want ErrMetadataUnavailable", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := st.RunnableRunbooks(cctx, scope); err == nil ||
		errors.Is(err, ErrNotFound) {
		t.Fatalf("cancelled lookup = %v, want an error that is not not-found", err)
	}
}
