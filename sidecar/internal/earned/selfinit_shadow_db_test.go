package earned

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Integration (real Postgres): the reconciler copies counted, scored
// shadow decisions of the monitored database (sage.shadow_decision) into
// the ledger as shadow evidence of their pair. Shadow evidence earns like
// real evidence of the family (distinct decisions), is labelled shadow in
// the class record and in promotion proposals, never demotes, and cannot
// alone carry a class to L3.

// newShadowFixture is a reconciler fixture with an empty shadow ledger
// and a ramp observed long enough for any promotion.
func newShadowFixture(t *testing.T) *fixture {
	t.Helper()
	f := newSelfFixture(t)
	clean := func() {
		if _, err := f.pool.Exec(f.ctx, `DELETE FROM sage.shadow_decision`); err != nil {
			t.Fatalf("clean shadow ledger: %v", err)
		}
	}
	clean()
	t.Cleanup(clean)
	start := f.clock.Now().Add(-90 * 24 * time.Hour)
	f.svc.WithRamp(func() RampFloor { return RampFloor{Start: start} })
	return f
}

// scoredShadow records a scored shadow decision in the monitored database.
func (f *fixture) scoredShadow(class ActionClass, fingerprint, score, source string,
	counted bool) int64 {
	f.t.Helper()
	var id int64
	family := SelfFamilyFor(class)
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.shadow_decision
		(database_name, fingerprint, family, action_class, title, sql, shape, prediction,
		 gate_verdict, gate_reason, trusted_verdict, trusted_reason, granted_level, status,
		 score, score_source, counted, score_reason, scored_at, recorded_at, last_seen_at)
		VALUES ($1, $2, $3, $4, 'shadow', 'VACUUM public.o', 'vacuum public.o', '{}',
		        'observe_only', 'autonomy_level', 'execute', 'autonomy_l3', 1, 'scored',
		        $5, $6, $7, 'test', clock_timestamp(), now() - interval '2 days',
		        now() - interval '1 day') RETURNING id`,
		f.db, fingerprint, string(family), string(class), score, source, counted).
		Scan(&id); err != nil {
		f.t.Fatalf("shadow decision: %v", err)
	}
	return id
}

func (f *fixture) shadowRows() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sage.trust_shadow_evidence
		WHERE deployment_id = $1 AND database_name = $2`, f.store.DeploymentID(), f.db).
		Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) record(c ActionClass) ClassRecord {
	f.t.Helper()
	r, err := f.store.ClassRecord(f.ctx, SelfFamilyFor(c), c)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *fixture) reconcile() ReconcileResult {
	f.t.Helper()
	res, err := NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx)
	if err != nil {
		f.t.Fatalf("reconcile: %v", err)
	}
	return res
}

func TestReconcilerRecordsOnlyCountedShadowScores(t *testing.T) {
	f := newShadowFixture(t)
	f.scoredShadow(ClassIndexCreate, "fp-hypo", "correct", "hypopg", true)
	f.scoredShadow(ClassIndexCreate, "fp-ext", "correct", "external", true)
	f.scoredShadow(ClassIndexCreate, "fp-op", "correct", "operator", false)
	f.scoredShadow(ClassIndexCreate, "fp-none", "unscored", "none", false)
	res := f.reconcile()
	if res.ShadowRecorded != 2 || f.shadowRows() != 2 {
		t.Fatalf("result %+v, evidence rows %d, want 2", res, f.shadowRows())
	}
	r := f.record(ClassIndexCreate)
	if r.ShadowCorrect != 2 || r.ShadowSuccesses != 2 || r.Successes != 2 ||
		r.Improved != 0 || r.ShadowIncorrect != 0 {
		t.Fatalf("record %+v", r)
	}
	if again := f.reconcile(); again.ShadowRecorded != 0 || f.shadowRows() != 2 {
		t.Fatalf("second pass %+v, rows %d", again, f.shadowRows())
	}
}

func TestShadowSuccessesCountDistinctDecisions(t *testing.T) {
	f := newShadowFixture(t)
	f.scoredShadow(ClassIndexCreate, "fp-same", "correct", "hypopg", true)
	f.scoredShadow(ClassIndexCreate, "fp-same", "correct", "hypopg", true)
	f.scoredShadow(ClassIndexCreate, "fp-other", "neutral", "external", true)
	f.reconcile()
	r := f.record(ClassIndexCreate)
	if r.ShadowCorrect != 2 || r.ShadowSuccesses != 1 || r.ShadowNeutral != 1 ||
		r.ShadowUncredited != 1 || r.Uncredited != 1 || r.Successes != 1 {
		t.Fatalf("a re-recorded decision counted twice: %+v", r)
	}
}

func TestShadowIncorrectResetsTheStreakButNeverDemotes(t *testing.T) {
	f := newShadowFixture(t)
	f.grandfatherAt(L3)
	for i := 0; i < 2; i++ {
		f.selfAction("create_index_concurrently",
			"CREATE INDEX CONCURRENTLY i_s ON public.orders (a)", "index_create", "success",
			"improved", "autonomy_l3", time.Minute)
	}
	f.reconcile()
	if r := f.record(ClassIndexCreate); r.Successes != 2 || r.ShadowSuccesses != 0 {
		t.Fatalf("real successes: %+v", r)
	}
	f.scoredShadow(ClassIndexCreate, "fp-bad", "incorrect", "hypopg", true)
	res := f.reconcile()
	r := f.record(ClassIndexCreate)
	if r.Successes != 0 || r.ShadowIncorrect != 1 || r.LastDemerit != CauseShadowIncorrect ||
		r.LastDemeritAt == nil {
		t.Fatalf("after an incorrect shadow decision: %+v", r)
	}
	st := f.state(FamilyTuning, ClassIndexCreate)
	if st.Level != L3 || st.Provenance != ProvenanceGrandfathered || res.Demoted != 0 {
		t.Fatalf("an incorrect shadow decision demoted the class: %+v (%+v)", st, res)
	}
	for _, e := range f.events(EventFilter{Family: FamilyTuning, Class: ClassIndexCreate,
		Database: f.db}) {
		if e.Type == EventAutoDowngraded {
			t.Fatalf("auto_downgraded event from shadow evidence: %+v", e)
		}
	}
}

func evaluateCreated(t *testing.T, f *fixture, c ActionClass) *Proposal {
	t.Helper()
	ev, err := f.svc.Evaluate(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range ev.Created {
		if ev.Created[i].Class == c && ev.Created[i].Family == SelfFamilyFor(c) {
			return &ev.Created[i]
		}
	}
	for _, np := range ev.NotProposed {
		if np.Class == c && np.Family == SelfFamilyFor(c) {
			t.Logf("%s not proposed: %s %+v", c, np.Reason, np.Unmet)
		}
	}
	return nil
}

func TestShadowEvidenceProposesL2AndAnAdminApproves(t *testing.T) {
	f := newShadowFixture(t)
	for _, fp := range []string{"a", "b", "c"} {
		f.scoredShadow(ClassIndexCreate, "fp-l2-"+fp, "correct", "hypopg", true)
	}
	f.reconcile()
	p := evaluateCreated(t, f, ClassIndexCreate)
	if p == nil || p.To != L2 {
		t.Fatalf("no L2 proposal from shadow evidence: %+v", p)
	}
	if st := f.state(FamilyTuning, ClassIndexCreate); st.Level != L1 {
		t.Fatalf("a proposal changed the level: %+v", st)
	}
	var body struct {
		Evidence struct {
			Record ClassRecord `json:"class_record"`
		} `json:"evidence"`
		Assessment Assessment `json:"assessment"`
	}
	if err := json.Unmarshal(p.Evidence, &body); err != nil {
		t.Fatal(err)
	}
	if body.Evidence.Record.ShadowSuccesses != 3 || body.Evidence.Record.Successes != 3 {
		t.Fatalf("proposal evidence does not show the shadow share: %+v",
			body.Evidence.Record)
	}
	c, _ := checkByName(body.Assessment, "class_successes")
	if !strings.Contains(c.Observed, "0 real, 3 shadow") {
		t.Fatalf("proposal check %+v must show real vs shadow", c)
	}
	st, err := f.svc.Approve(f.ctx, p.ID, "user:1:admin@example.com", "shadow reviewed")
	if err != nil || st.Level != L2 {
		t.Fatalf("approve: %+v (%v)", st, err)
	}
}

func TestShadowEvidenceAloneNeverReachesL3(t *testing.T) {
	f := newShadowFixture(t)
	for i := 0; i < 3; i++ {
		f.scoredShadow(ClassIndexCreate, "fp-x"+string(rune('a'+i)), "correct", "hypopg", true)
	}
	f.reconcile()
	if p := evaluateCreated(t, f, ClassIndexCreate); p == nil {
		t.Fatal("no L2 proposal")
	} else if _, err := f.svc.Approve(f.ctx, p.ID, "user:1:admin@example.com",
		"ok"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		f.scoredShadow(ClassIndexCreate, "fp-y"+string(rune('a'+i)), "correct", "external",
			true)
	}
	f.reconcile()
	if p := evaluateCreated(t, f, ClassIndexCreate); p != nil {
		t.Fatalf("L3 proposed from shadow evidence alone: %+v", p)
	}
	for i := 0; i < MinRealSuccessesL3; i++ {
		f.selfAction("create_index_concurrently",
			"CREATE INDEX CONCURRENTLY i_r ON public.orders (r)", "index_create", "success",
			"improved", "operator_approved", time.Minute)
	}
	f.reconcile()
	p := evaluateCreated(t, f, ClassIndexCreate)
	if p == nil || p.To != L3 {
		t.Fatalf("3 real + capped shadow successes: no L3 proposal (%+v)", p)
	}
	var body struct {
		Assessment Assessment `json:"assessment"`
	}
	if err := json.Unmarshal(p.Evidence, &body); err != nil {
		t.Fatal(err)
	}
	c, _ := checkByName(body.Assessment, "class_successes")
	if !strings.HasPrefix(c.Observed, "10 (3 real, 7 shadow") {
		t.Fatalf("L3 proposal successes %q", c.Observed)
	}
}

func TestConcurrentReconcilersRecordShadowEvidenceOnce(t *testing.T) {
	f := newShadowFixture(t)
	for i := 0; i < 5; i++ {
		f.scoredShadow(ClassVacuum, "fp-c"+string(rune('a'+i)), "correct", "external", true)
	}
	var wg sync.WaitGroup
	total := make([]int, 4)
	for i := range total {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx)
			if err == nil {
				total[i] = res.ShadowRecorded
			}
		}(i)
	}
	wg.Wait()
	sum := 0
	for _, n := range total {
		sum += n
	}
	if sum != 5 || f.shadowRows() != 5 {
		t.Fatalf("recorded %d (%v), rows %d, want 5", sum, total, f.shadowRows())
	}
	if r := f.record(ClassVacuum); r.ShadowSuccesses != 5 {
		t.Fatalf("record %+v", r)
	}
}

func TestTrustViewShowsShadowCounts(t *testing.T) {
	f := newShadowFixture(t)
	f.scoredShadow(ClassIndexDrop, "fp-d1", "correct", "external", true)
	f.scoredShadow(ClassIndexDrop, "fp-d2", "incorrect", "external", true)
	f.scoredShadow(ClassIndexDrop, "fp-d3", "neutral", "external", true)
	f.reconcile()
	v, err := f.svc.TrustView(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range v.Rows {
		if row.Family != FamilyHygiene || row.Class != ClassIndexDrop {
			continue
		}
		e := row.Evidence
		if e.ShadowCorrect != 1 || e.ShadowIncorrect != 1 || e.ShadowNeutral != 1 {
			t.Fatalf("trust row evidence %+v", e)
		}
		return
	}
	t.Fatal("no hygiene/index_drop row")
}

func TestLimiterGrantedLevel(t *testing.T) {
	f := newShadowFixture(t)
	lim := f.svc.Limiter(Binding{Database: f.db})
	req := policy.ActionRequest{SQL: "VACUUM public.o",
		Contract: &policy.ActionContract{ActionType: "vacuum_table"}}
	got, err := lim.GrantedLevel(f.ctx, req)
	if err != nil || got != int(L1) {
		t.Fatalf("default vacuum level %d (%v), want 1", got, err)
	}
	f.grandfatherAt(L3)
	if got, err := lim.GrantedLevel(f.ctx, req); err != nil || got != int(L3) {
		t.Fatalf("grandfathered vacuum level %d (%v), want 3", got, err)
	}
	if _, err := lim.GrantedLevel(f.ctx, policy.ActionRequest{SQL: "SELECT 1"}); err == nil {
		t.Fatal("a request without a ledger pair has no granted level")
	}
}
