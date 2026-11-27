package earned

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// llm-01 on PostgreSQL: the ledger selects, per family, the newest
// measurement taken with the model the investigator runs, so a newer
// report from another model never shadows a matching one, and a change
// of the running model is one recorded flip in the trust history.

// ingestModelLift ingests a live lift report measured with model.
func (f *fixture) ingestModelLift(at time.Time, model string, recs ...string) EvalRun {
	f.t.Helper()
	return f.ingestSignedLift(liftReportModel(at, model, recs...))
}

// runningModel is a running-model source the test can switch.
type runningModel struct {
	mu    sync.Mutex
	model string
}

func (r *runningModel) get() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.model
}

func (r *runningModel) set(m string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.model = m
}

func TestModelLiftStore_MatchingModelIsNotShadowedByANewerReport(t *testing.T) {
	f := newFixture(t)
	older := f.ingestModelLift(f.clock.Now().Add(-3*time.Hour), "gpt-6",
		good("lock_blocking", 16, 16))
	newer := f.ingestModelLift(f.clock.Now().Add(-time.Hour), "llama3",
		good("lock_blocking", 20, 20))
	for running, want := range map[string]struct {
		report  string
		granted bool
	}{
		"gpt-6":        {older.ID, true},
		"OpenAI/GPT-6": {older.ID, true},
		"llama3":       {newer.ID, true},
		"gpt-6-luna":   {newer.ID, false},
	} {
		got, err := f.svc.ModelRootAuthority(f.ctx, FamilyLockBlocking, running)
		if err != nil {
			t.Fatalf("%s: %v", running, err)
		}
		if got.Report == nil || got.Report.ID != want.report || got.Granted != want.granted {
			t.Errorf("%s: authority %+v, want report %s granted %v", running, got,
				want.report, want.granted)
		}
	}
	got, err := f.svc.ModelRootAuthority(f.ctx, FamilyLockBlocking, "gpt-6-luna")
	if err != nil || !strings.Contains(got.Reason, `measured with "llama3"; the `+
		`investigator runs "gpt-6-luna"`) {
		t.Fatalf("no matching report: %+v (%v), want the newest named as a mismatch", got, err)
	}
}

func TestModelLiftStore_ReportWithoutAModelNeverShadowsAMatchingOne(t *testing.T) {
	f := newFixture(t)
	match := f.ingestModelLift(f.clock.Now().Add(-3*time.Hour), "gpt-6",
		good("lock_blocking", 16, 16))
	f.ingestModelLift(f.clock.Now().Add(-time.Hour), "", good("lock_blocking", 40, 40))
	got, err := f.svc.ModelRootAuthority(f.ctx, FamilyLockBlocking, "gpt-6")
	if err != nil || !got.Granted || got.Report == nil || got.Report.ID != match.ID {
		t.Fatalf("authority %+v (%v), want the gpt-6 report %s", got, err, match.ID)
	}
	got, err = f.svc.ModelRootAuthority(f.ctx, FamilyLockBlocking, "llama3")
	if err != nil || got.Granted || !strings.Contains(got.Reason, "names no model") {
		t.Fatalf("unmatched running model: %+v (%v), want the newest, unnamed one", got, err)
	}
}

func TestModelLiftStore_ChangingTheRunningModelIsOneRecordedFlip(t *testing.T) {
	f := newFixture(t)
	run := f.ingestModelLift(f.clock.Now().Add(-time.Hour), "gpt-6",
		good("lock_blocking", 16, 16))
	src := &runningModel{model: "gpt-6"}
	f.svc.WithRunningModel(src.get)
	if _, err := f.svc.ReconcileRootAuthority(f.ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if evs := f.rootEvents(FamilyLockBlocking); len(evs) != 1 ||
		evs[0].Type != EventRootAuthorityGranted {
		t.Fatalf("history after the grant = %+v", evs)
	}
	src.set("gpt-6-luna") // a hot reload to another model
	got, err := f.svc.ModelRootAuthority(f.ctx, FamilyLockBlocking, src.get())
	if err != nil || got.Granted || got.Status != RootAdvisory {
		t.Fatalf("after the reload: %+v (%v), want advisory", got, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := f.svc.ReconcileRootAuthority(f.ctx); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	evs := f.rootEvents(FamilyLockBlocking)
	if len(evs) != 2 || evs[1].Type != EventRootAuthorityRevoked {
		t.Fatalf("history after the reload = %+v, want granted then one revoked", evs)
	}
	if !strings.Contains(evs[1].Reason, `the investigator runs "gpt-6-luna"`) ||
		!strings.Contains(string(evs[1].Evidence), run.ID) {
		t.Fatalf("revocation %q %s must name the mismatch and the report", evs[1].Reason,
			evs[1].Evidence)
	}
}

func TestModelLiftView_ShowsTheMeasuredAndRunningModel(t *testing.T) {
	f := newFixture(t)
	f.ingestModelLift(f.clock.Now().Add(-time.Hour), "OpenAI/GPT-6",
		good("lock_blocking", 16, 16))
	src := &runningModel{model: "gpt-6-luna"}
	f.svc.WithRunningModel(src.get)
	v, err := f.svc.ModelLiftView(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	lock := viewFamily(t, v, FamilyLockBlocking)
	if lock.MeasuredModel != "gpt-6" || lock.RunningModel != "gpt-6-luna" || lock.Granted ||
		!strings.Contains(lock.Reason, "the investigator runs") {
		t.Fatalf("lock_blocking = %+v", lock)
	}
	if wal := viewFamily(t, v, FamilyWAL); wal.RunningModel != "gpt-6-luna" ||
		wal.MeasuredModel != "" {
		t.Fatalf("an unmeasured family still names the running model: %+v", wal)
	}
	src.set("gpt-6")
	if v, err = f.svc.ModelLiftView(f.ctx); err != nil || !viewFamily(t, v,
		FamilyLockBlocking).Granted {
		t.Fatalf("view after switching to the measured model: %+v (%v)", v, err)
	}
}

func TestModelLiftView_WithoutARunningModelIsAdvisory(t *testing.T) {
	f := newFixture(t)
	f.ingestModelLift(f.clock.Now().Add(-time.Hour), "gpt-6", good("lock_blocking", 16, 16))
	f.svc.WithRunningModel(nil)
	v, err := f.svc.ModelLiftView(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	lock := viewFamily(t, v, FamilyLockBlocking)
	if lock.Granted || !strings.Contains(lock.Reason, "the investigator's model is unknown") {
		t.Fatalf("lock_blocking without a running model = %+v", lock)
	}
}

func viewFamily(t *testing.T, v ModelLiftView, family Family) RootAuthority {
	t.Helper()
	for _, a := range v.Families {
		if a.Family == family {
			return a
		}
	}
	t.Fatalf("family %s missing from the view", family)
	return RootAuthority{}
}

func TestModelLiftStore_LegacyRowWithoutTheKeyNeverShadowsAMatchingOne(t *testing.T) {
	// A lift stored before llm-01 has no llm_model key at all, so the
	// match expression is NULL there; NULL must not sort ahead of a match.
	f := newFixture(t)
	match := f.ingestModelLift(f.clock.Now().Add(-3*time.Hour), "gpt-6",
		good("lock_blocking", 16, 16))
	legacy := f.ingestModelLift(f.clock.Now().Add(-time.Hour), "gpt-6",
		good("lock_blocking", 40, 40))
	tag, err := f.pool.Exec(f.ctx, `UPDATE sage.sre_eval_runs
		SET model_lift = model_lift - 'llm_model' WHERE id = $1`, legacy.ID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("strip llm_model from %s: %v (%d rows)", legacy.ID, err, tag.RowsAffected())
	}
	got, err := f.svc.ModelRootAuthority(f.ctx, FamilyLockBlocking, "gpt-6")
	if err != nil || !got.Granted || got.Report == nil || got.Report.ID != match.ID {
		t.Fatalf("authority %+v (%v), want the gpt-6 report %s", got, err, match.ID)
	}
	got, err = f.svc.ModelRootAuthority(f.ctx, FamilyLockBlocking, "llama3")
	if err != nil || got.Granted || got.Report == nil || got.Report.ID != legacy.ID ||
		!strings.Contains(got.Reason, "names no model") {
		t.Fatalf("unmatched model: %+v (%v), want the newest, legacy report advisory", got, err)
	}
}

// benches-09 (security): only a signed release or local-run measurement
// stamped with the running build grants model-root authority. An unsigned
// operator upload with a winning model_lift, stamped or not, and a signed
// row without a build stamp (a legacy row) leave ReconcileRootAuthority
// unchanged; a signed, stamped one grants.

// winningLift is a stamped (when b is known) live lift that passes the bar.
func winningLift(b Build) []byte {
	raw := liftReport(fixtureEpoch.Add(-time.Hour), "live", false, good("lock_blocking", 16, 16))
	if b.Known() {
		return stampLift(raw, b)
	}
	return raw
}

// assertNoGrant fails when reconciling grants, or the family reads granted.
func (f *fixture) assertNoGrant(what string) {
	f.t.Helper()
	changes, err := f.svc.ReconcileRootAuthority(f.ctx)
	if err != nil {
		f.t.Fatalf("%s: reconcile: %v", what, err)
	}
	for _, c := range changes {
		if c.Granted {
			f.t.Fatalf("%s granted model-root authority: %+v", what, c)
		}
	}
	got, err := f.svc.ModelRootAuthority(f.ctx, FamilyLockBlocking, fixtureModel)
	if err != nil || got.Granted || got.Report != nil {
		f.t.Fatalf("%s: authority %+v (%v), want advisory with no deciding report", what,
			got, err)
	}
	for _, ev := range f.rootEvents(FamilyLockBlocking) {
		if ev.Type == EventRootAuthorityGranted {
			f.t.Fatalf("%s recorded a grant: %+v", what, ev)
		}
	}
}

func TestModelLiftStore_UnsignedOperatorUploadNeverGrants(t *testing.T) {
	for name, b := range map[string]Build{"stamped": buildA, "unstamped": {}} {
		f := newBuildFixture(t, newUUID(t), buildA)
		run, err := f.svc.IngestEvalRun(f.ctx, winningLift(b), SourceBench, "admin", "")
		if err != nil || run.Origin != OriginOperator || run.ModelLift == nil {
			t.Fatalf("%s operator upload = %+v (%v)", name, run, err)
		}
		f.assertNoGrant(name + " unsigned operator upload")
	}
}

func TestModelLiftStore_SignedButUnstampedRowNeverGrants(t *testing.T) {
	f := newBuildFixture(t, newUUID(t), buildA)
	if _, err := f.ingestSigned(winningLift(Build{}), commitA); !errors.Is(err,
		ErrBuildMismatch) {
		t.Fatalf("a signed unstamped report = %v, want it refused (ErrBuildMismatch)", err)
	}
	run, err := f.ingestSigned(winningLift(buildA), commitA)
	if err != nil {
		t.Fatal(err)
	}
	// A row stored without a stamp (an older writer) is what the dropped
	// branch of the lift query used to accept.
	tag, err := f.pool.Exec(f.ctx, `UPDATE sage.sre_eval_runs
		SET pg_sage_version = '', pg_sage_commit = '' WHERE id = $1`, run.ID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("unstamp %s: %v (%d rows)", run.ID, err, tag.RowsAffected())
	}
	f.assertNoGrant("a signed unstamped row")
}

func TestModelLiftStore_SignedStampedAndLocalRunGrant(t *testing.T) {
	for _, origin := range []string{OriginSignedRelease, OriginLocalRun} {
		f := newBuildFixture(t, newUUID(t), buildA)
		in := BenchIngest{Origin: origin, Actor: "release_bench", RequireBuild: true}
		if origin == OriginSignedRelease {
			in.Signature = signatureFor(commitA)
		}
		run, err := f.svc.IngestBench(f.ctx, winningLift(buildA), in)
		if err != nil {
			t.Fatalf("%s: ingest: %v", origin, err)
		}
		changes, err := f.svc.ReconcileRootAuthority(f.ctx)
		if err != nil || len(changes) != 1 || !changes[0].Granted ||
			changes[0].ReportID != run.ID || changes[0].Family != FamilyLockBlocking {
			t.Fatalf("%s: reconcile = %+v (%v), want one grant by %s", origin, changes, err,
				run.ID)
		}
		got, err := f.svc.ModelRootAuthority(f.ctx, FamilyLockBlocking, fixtureModel)
		if err != nil || !got.Granted || got.Report == nil || got.Report.Origin != origin {
			t.Fatalf("%s: authority %+v (%v)", origin, got, err)
		}
	}
}

// An unsigned upload newer than a signed measurement never shadows it.
func TestModelLiftStore_NewerUnsignedUploadDoesNotShadowASignedOne(t *testing.T) {
	f := newBuildFixture(t, newUUID(t), buildA)
	signed, err := f.ingestSigned(stampLift(liftReport(fixtureEpoch.Add(-3*time.Hour), "live",
		false, good("lock_blocking", 16, 16)), buildA), commitA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.IngestEvalRun(f.ctx, stampLift(liftReport(fixtureEpoch.Add(-time.Hour),
		"live", false, good("lock_blocking", 1, 20)), buildA), SourceBench, "admin",
		""); err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.ModelRootAuthority(f.ctx, FamilyLockBlocking, fixtureModel)
	if err != nil || !got.Granted || got.Report == nil || got.Report.ID != signed.ID {
		t.Fatalf("authority %+v (%v), want the signed report %s", got, err, signed.ID)
	}
}

// The running build is unstamped (a dev build): no lift row matches it.
func TestModelLiftStore_UnstampedRunningBuildNeverGrants(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.IngestBench(f.ctx, winningLift(Build{}), BenchIngest{
		Origin: OriginOperator, Actor: "admin"}); err != nil {
		t.Fatal(err)
	}
	f.assertNoGrant("an unstamped report on an unstamped build")
}
