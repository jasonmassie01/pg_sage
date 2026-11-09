package earned

import (
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
	run, err := f.svc.IngestEvalRun(f.ctx, liftReportModel(at, model, recs...),
		SourceBench, "admin", "")
	if err != nil {
		f.t.Fatalf("ingest %q lift report: %v", model, err)
	}
	return run
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
