package earned

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// earned-07: a self-initiated L3 earned on one PostgreSQL major is capped
// at L2 when the monitored server runs another, until three improved
// outcomes on the new major or an admin's re-approval re-validate it.
// Incident families, carried-over levels and the demerit path are not
// touched.

// fakeMajor is a settable server major source.
type fakeMajor struct {
	mu    sync.Mutex
	major int
	err   error
}

func (m *fakeMajor) get(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.major, m.err
}

func (m *fakeMajor) set(major int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.major, m.err = major, err
}

type majorFixture struct {
	*limiterFixture
	major *fakeMajor
}

func newMajorFixture(t *testing.T, major int) *majorFixture {
	t.Helper()
	lf := newLimiterFixture(t)
	m := &fakeMajor{major: major}
	src := MajorSource(m.get)
	lf.svc.WithServerMajor(src)
	lf.lim = lf.svc.Limiter(Binding{Database: "orders", Budget: lf.budget, HA: lf.ha,
		Concurrency: lf.conc, ServerMajor: src})
	return &majorFixture{limiterFixture: lf, major: m}
}

// seedSelfL3 grandfathers every reversible self-initiated class at L3.
func (mf *majorFixture) seedSelfL3() {
	mf.t.Helper()
	if _, err := mf.svc.SeedGrandfathered(mf.ctx, mf.db,
		rampBound(mf.clock.Now(), 60*24*time.Hour)); err != nil {
		mf.t.Fatalf("seed grandfathered: %v", err)
	}
}

// storedMajor is the pair's server_major column (nil for NULL).
func (mf *majorFixture) storedMajor(f Family, c ActionClass) *int {
	mf.t.Helper()
	var major *int16
	if err := mf.pool.QueryRow(mf.ctx, `SELECT server_major FROM sage.sre_family_autonomy
		WHERE deployment_id = $1 AND database_name = $2 AND family = $3
		  AND action_class = $4`, mf.store.DeploymentID(), mf.db, string(f),
		string(c)).Scan(&major); err != nil {
		mf.t.Fatalf("read server_major: %v", err)
	}
	if major == nil {
		return nil
	}
	v := int(*major)
	return &v
}

func (mf *majorFixture) clearMajor(f Family, c ActionClass) {
	mf.t.Helper()
	if _, err := mf.pool.Exec(mf.ctx, `UPDATE sage.sre_family_autonomy SET server_major = NULL
		WHERE deployment_id = $1 AND database_name = $2 AND family = $3
		  AND action_class = $4`, mf.store.DeploymentID(), mf.db, string(f),
		string(c)); err != nil {
		mf.t.Fatalf("clear server_major: %v", err)
	}
}

// capEvents are the pair's capped and cap_cleared entries, oldest first.
func (mf *majorFixture) capEvents(f Family, c ActionClass) []Event {
	mf.t.Helper()
	var out []Event
	evs := mf.events(EventFilter{Family: f, Class: c, Database: mf.db})
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == EventCapped || evs[i].Type == EventCapCleared {
			out = append(out, evs[i])
		}
	}
	return out
}

// improved records n improved vacuum outcomes, a minute apart, from id.
func (mf *majorFixture) improved(n int, id int64) {
	mf.t.Helper()
	for i := 0; i < n; i++ {
		mf.clock.Advance(time.Minute)
		if err := mf.svc.RecordOutcome(mf.ctx, Outcome{Database: mf.db,
			ActionLogID: id + int64(i), Family: FamilyHygiene, Class: ClassVacuum, Level: L2,
			Result: ResultVerifiedRecovery, Verdict: "improved", Source: SourceExecutor,
			Actor: ActorPgSage}); err != nil {
			mf.t.Fatalf("record improved outcome: %v", err)
		}
	}
}

func intIs(p *int, want int) bool { return p != nil && *p == want }

func TestMajorCapCapsAtL2AndClearsAfterImprovedOutcomes(t *testing.T) {
	mf := newMajorFixture(t, 16)
	mf.seedSelfL3()
	if got := mf.storedMajor(FamilyHygiene, ClassVacuum); !intIs(got, 16) {
		t.Fatalf("grandfathered row server_major = %v, want 16", got)
	}
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 3 {
		t.Fatalf("same major = %+v, want L3", got)
	}
	mf.major.set(17, nil)
	got := mf.limit(selfVacuum("public.orders"))
	if got.Level != 2 || got.Granted != 3 || got.Downgraded ||
		!strings.Contains(strings.Join(got.Reasons, ","), "server_major_changed:16->17") {
		t.Fatalf("after 16->17 = %+v, want L2 of granted L3", got)
	}
	mf.limit(selfVacuum("public.orders"))
	evs := mf.capEvents(FamilyHygiene, ClassVacuum)
	if len(evs) != 1 || evs[0].Type != EventCapped ||
		evs[0].Reason != "server_major_changed:16->17" {
		t.Fatalf("cap events = %+v, want one capped", evs)
	}
	mf.improved(2, 7100)
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 2 {
		t.Fatalf("after 2 improved = %+v, want L2", got)
	}
	mf.improved(1, 7200)
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 3 || len(got.Reasons) != 0 {
		t.Fatalf("after 3 improved = %+v, want L3", got)
	}
	evs = mf.capEvents(FamilyHygiene, ClassVacuum)
	if len(evs) != 2 || evs[1].Type != EventCapCleared {
		t.Fatalf("cap events = %+v, want capped then cap_cleared", evs)
	}
	if got := mf.storedMajor(FamilyHygiene, ClassVacuum); !intIs(got, 17) {
		t.Fatalf("server_major after re-validation = %v, want 17", got)
	}
	// Another class of the family was not re-validated by vacuum's outcomes.
	if got := mf.limit(selfReq("analyze_table", "ANALYZE public.orders")); got.Level != 2 {
		t.Fatalf("analyze after vacuum re-validated = %+v, want L2", got)
	}
}

// Outcomes recorded before the cap event do not count.
func TestMajorCapCountsOnlyOutcomesAfterTheCap(t *testing.T) {
	mf := newMajorFixture(t, 16)
	mf.seedSelfL3()
	mf.improved(5, 7300)
	mf.major.set(17, nil)
	mf.clock.Advance(time.Minute)
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 2 {
		t.Fatalf("earlier improved outcomes cleared the cap: %+v", got)
	}
	mf.clock.Advance(time.Minute)
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 2 {
		t.Fatalf("second call = %+v, want L2", got)
	}
}

func TestMajorCapReapprovalClearsAtOnce(t *testing.T) {
	mf := newMajorFixture(t, 16)
	mf.seedSelfL3()
	mf.major.set(17, nil)
	mf.limit(selfVacuum("public.orders"))
	st, err := mf.lim.Revalidate(mf.ctx, FamilyHygiene, ClassVacuum,
		"user:1:admin@example.com", "checked the 17 planner")
	if err != nil || !intIs(st.ServerMajor, 17) || st.Level != L3 {
		t.Fatalf("revalidate = %+v, %v", st, err)
	}
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 3 {
		t.Fatalf("after re-approval = %+v, want L3", got)
	}
	evs := mf.capEvents(FamilyHygiene, ClassVacuum)
	last := evs[len(evs)-1]
	if len(evs) != 2 || last.Type != EventCapCleared ||
		last.Actor != "user:1:admin@example.com" ||
		!strings.Contains(last.Reason, "checked the 17 planner") {
		t.Fatalf("cap events = %+v", evs)
	}
	if _, err := mf.lim.Revalidate(mf.ctx, FamilyHygiene, ClassVacuum,
		"user:1:admin@example.com", ""); !errors.Is(err, ErrNotCapped) {
		t.Fatalf("second revalidate = %v, want ErrNotCapped", err)
	}
}

func TestMajorCapRevalidateRefusals(t *testing.T) {
	mf := newMajorFixture(t, 16)
	mf.seedSelfL3()
	mf.major.set(17, nil)
	admin := "user:1:admin@example.com"
	for name, c := range map[string]struct {
		f     Family
		class ActionClass
		actor string
		want  error
	}{
		"incident pair":  {FamilyWraparound, ClassFreeze, admin, ErrInvalidRequest},
		"unknown class":  {FamilyHygiene, "nope", admin, ErrInvalidRequest},
		"wrong family":   {FamilyTuning, ClassVacuum, admin, ErrInvalidRequest},
		"pg_sage itself": {FamilyHygiene, ClassVacuum, ActorPgSage, ErrHumanApprovalRequired},
		"no actor":       {FamilyHygiene, ClassVacuum, " ", ErrHumanApprovalRequired},
		"below L3":       {FamilyHygiene, ClassRetention, admin, ErrNotCapped},
	} {
		if _, err := mf.lim.Revalidate(mf.ctx, c.f, c.class, c.actor, ""); !errors.Is(err,
			c.want) {
			t.Errorf("%s: revalidate = %v, want %v", name, err, c.want)
		}
	}
	if got := mf.storedMajor(FamilyHygiene, ClassVacuum); !intIs(got, 16) {
		t.Fatalf("a refused revalidate changed server_major to %v", got)
	}
	mf.major.set(0, errors.New("probe down"))
	if _, err := mf.lim.Revalidate(mf.ctx, FamilyHygiene, ClassVacuum, admin,
		""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("revalidate without a version = %v, want ErrUnavailable", err)
	}
	other := mf.svc.Limiter(Binding{Database: "billing", ServerMajor: mf.lim.b.ServerMajor})
	if _, err := other.Revalidate(mf.ctx, FamilyHygiene, ClassVacuum, admin,
		""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("revalidate through another database's limiter = %v", err)
	}
}

// Nil/zero: a row from before earned-07 has no major; it is stamped with
// today's and keeps L3 without an event.
func TestMajorCapLegacyRowIsStampedWithoutCap(t *testing.T) {
	mf := newMajorFixture(t, 17)
	mf.seedSelfL3()
	mf.clearMajor(FamilyHygiene, ClassVacuum)
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 3 {
		t.Fatalf("legacy row = %+v, want L3", got)
	}
	if got := mf.storedMajor(FamilyHygiene, ClassVacuum); !intIs(got, 17) {
		t.Fatalf("legacy row server_major = %v, want 17", got)
	}
	if evs := mf.capEvents(FamilyHygiene, ClassVacuum); len(evs) != 0 {
		t.Fatalf("stamping recorded %+v", evs)
	}
}

// Invalid input: an unreadable version neither caps nor stamps.
func TestMajorCapUnreadableVersionChangesNothing(t *testing.T) {
	mf := newMajorFixture(t, 16)
	mf.seedSelfL3()
	mf.clearMajor(FamilyHygiene, ClassAnalyze)
	mf.major.set(0, errors.New("server_version_num unreadable"))
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 3 {
		t.Fatalf("unreadable version = %+v, want L3 (no cap)", got)
	}
	if got := mf.limit(selfReq("analyze_table", "ANALYZE public.orders")); got.Level != 3 {
		t.Fatalf("unreadable version on a legacy row = %+v", got)
	}
	if got := mf.storedMajor(FamilyHygiene, ClassVacuum); !intIs(got, 16) {
		t.Fatalf("server_major = %v, want 16 unchanged", got)
	}
	if got := mf.storedMajor(FamilyHygiene, ClassAnalyze); got != nil {
		t.Fatalf("legacy server_major = %v, want NULL unchanged", *got)
	}
	if evs := mf.capEvents(FamilyHygiene, ClassVacuum); len(evs) != 0 {
		t.Fatalf("events = %+v", evs)
	}
}

// Error propagation: a stamp that loses the version race writes nothing
// and is retried by the next call, which is not a cap.
func TestMajorCapStampConflictIsRetried(t *testing.T) {
	mf := newMajorFixture(t, 17)
	mf.seedSelfL3()
	mf.clearMajor(FamilyHygiene, ClassVacuum)
	st, err := mf.svc.Granted(mf.ctx, FamilyHygiene, ClassVacuum)
	if err != nil {
		t.Fatal(err)
	}
	stale := st
	stale.Version++
	ok, err := mf.lim.stampMajor(mf.ctx, stale, nil, 17)
	if err != nil || ok {
		t.Fatalf("stale stamp = %v, %v; want no row and no error", ok, err)
	}
	if got := mf.storedMajor(FamilyHygiene, ClassVacuum); got != nil {
		t.Fatalf("stale stamp wrote %v", *got)
	}
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 3 {
		t.Fatalf("retry = %+v, want L3", got)
	}
	if got := mf.storedMajor(FamilyHygiene, ClassVacuum); !intIs(got, 17) {
		t.Fatalf("retried stamp = %v, want 17", got)
	}
	if evs := mf.capEvents(FamilyHygiene, ClassVacuum); len(evs) != 0 {
		t.Fatalf("events = %+v", evs)
	}
}

// Concurrency: parallel authorizations on a changed major record one cap.
func TestMajorCapConcurrentLimitsRecordOneCap(t *testing.T) {
	mf := newMajorFixture(t, 16)
	mf.seedSelfL3()
	mf.major.set(17, nil)
	var wg sync.WaitGroup
	levels := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := mf.lim.Limit(mf.ctx, selfVacuum("public.orders"))
			if err != nil {
				levels <- -1
				return
			}
			levels <- got.Level
		}()
	}
	wg.Wait()
	close(levels)
	for l := range levels {
		if l != 2 {
			t.Fatalf("a parallel limit = %d, want 2", l)
		}
	}
	if evs := mf.capEvents(FamilyHygiene, ClassVacuum); len(evs) != 1 {
		t.Fatalf("cap events = %+v, want exactly one", evs)
	}
}

// Boundary: only L3 is capped; an L2 earned on 16 keeps L2 on 17.
func TestMajorCapLeavesLowerLevelsAlone(t *testing.T) {
	mf := newMajorFixture(t, 16)
	mf.seedLevel(FamilyHygiene, ClassVacuum, L2)
	mf.major.set(17, nil)
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 2 || len(got.Reasons) != 0 {
		t.Fatalf("L2 on a new major = %+v", got)
	}
	if got := mf.storedMajor(FamilyHygiene, ClassVacuum); !intIs(got, 16) {
		t.Fatalf("an L2 row was restamped: %v", got)
	}
	if evs := mf.capEvents(FamilyHygiene, ClassVacuum); len(evs) != 0 {
		t.Fatalf("events = %+v", evs)
	}
}

// Must not change: incident families keep their evidence rule.
func TestMajorCapLeavesIncidentFamiliesAlone(t *testing.T) {
	mf := newMajorFixture(t, 16)
	mf.seedL3()
	mf.major.set(17, nil)
	got := mf.limit(freezeRequest(mf.clock.Now()))
	if got.Level != 3 || strings.Contains(strings.Join(got.Reasons, ","), "server_major") {
		t.Fatalf("incident L3 on a new major = %+v", got)
	}
	for _, e := range mf.capEvents(FamilyWraparound, ClassFreeze) {
		if strings.Contains(e.Reason, "server_major") {
			t.Fatalf("incident pair capped by the major: %+v", e)
		}
	}
}

// Level writes record the major they were made on.
func TestMajorCapLevelWritesRecordTheMajor(t *testing.T) {
	mf := newMajorFixture(t, 16)
	mf.seedSelfL3()
	mf.major.set(17, nil)
	if _, err := mf.svc.Downgrade(mf.ctx, DowngradeRequest{Family: FamilyHygiene,
		Class: ClassVacuum, To: L2, Actor: "user:2:ops@example.com",
		Reason: "after the upgrade"}); err != nil {
		t.Fatal(err)
	}
	if got := mf.storedMajor(FamilyHygiene, ClassVacuum); !intIs(got, 17) {
		t.Fatalf("downgrade server_major = %v, want 17", got)
	}
	// A write while the version is unreadable keeps the stored major.
	mf.major.set(0, errors.New("down"))
	if _, err := mf.svc.Downgrade(mf.ctx, DowngradeRequest{Family: FamilyHygiene,
		Class: ClassVacuum, To: L1, Actor: "user:2:ops@example.com",
		Reason: "again"}); err != nil {
		t.Fatal(err)
	}
	if got := mf.storedMajor(FamilyHygiene, ClassVacuum); !intIs(got, 17) {
		t.Fatalf("unreadable write server_major = %v, want 17 kept", got)
	}
	// Without a source a seed writes NULL (stamped later).
	g := newFixture(t)
	if _, err := g.svc.SeedGrandfathered(g.ctx, g.db,
		rampBound(g.clock.Now(), 60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	st, err := g.svc.Granted(g.ctx, FamilyHygiene, ClassVacuum)
	if err != nil || st.ServerMajor != nil || st.Level != L3 {
		t.Fatalf("seed without a source = %+v, %v", st, err)
	}
}

func TestMajorCapSweep(t *testing.T) {
	mf := newMajorFixture(t, 16)
	mf.seedSelfL3()
	mf.major.set(17, nil)
	res, err := mf.lim.Sweep(mf.ctx, nil)
	if err != nil || res.SelfPairs != len(selfClassesSeededAtL3) ||
		res.Capped != len(selfClassesSeededAtL3) {
		t.Fatalf("sweep = %+v, %v; want %d self pairs capped", res, err,
			len(selfClassesSeededAtL3))
	}
	if res, err = mf.lim.Sweep(mf.ctx, nil); err != nil || res.Capped != 0 {
		t.Fatalf("second sweep = %+v, %v; want nothing new", res, err)
	}
	mf.improved(3, 7400)
	res, err = mf.lim.Sweep(mf.ctx, nil)
	if err != nil || res.Cleared != 1 {
		t.Fatalf("sweep after 3 improved = %+v, %v; want vacuum cleared", res, err)
	}
	if got := mf.storedMajor(FamilyHygiene, ClassVacuum); !intIs(got, 17) {
		t.Fatalf("swept server_major = %v", got)
	}
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 3 {
		t.Fatalf("after the sweep cleared it = %+v", got)
	}
}

func TestMajorCapTrustRow(t *testing.T) {
	mf := newMajorFixture(t, 16)
	mf.seedSelfL3()
	mf.major.set(17, nil)
	mf.limit(selfVacuum("public.orders"))
	mf.improved(1, 7500)
	v, err := mf.svc.TrustView(mf.ctx)
	if err != nil {
		t.Fatal(err)
	}
	mf.lim.AnnotateTrust(mf.ctx, &v)
	var vac, retention *TrustRow
	for i := range v.Rows {
		switch {
		case v.Rows[i].Family == FamilyHygiene && v.Rows[i].Class == ClassVacuum:
			vac = &v.Rows[i]
		case v.Rows[i].Family == FamilyHygiene && v.Rows[i].Class == ClassRetention:
			retention = &v.Rows[i]
		}
	}
	if vac == nil || vac.MajorCap == nil || vac.Effective == nil || *vac.Effective != L2 {
		t.Fatalf("vacuum row = %+v", vac)
	}
	want := "capped at L2: PostgreSQL 16→17; 1 of 3 improved outcomes, or re-approve"
	if vac.MajorCap.Text != want || vac.MajorCap.From != 16 || vac.MajorCap.To != 17 ||
		vac.MajorCap.Improved != 1 || vac.MajorCap.Needed != 3 {
		t.Fatalf("vacuum cap = %+v", vac.MajorCap)
	}
	if retention == nil || retention.MajorCap != nil {
		t.Fatalf("retention (below L3) = %+v", retention)
	}
	view, err := mf.svc.View(mf.ctx)
	if err != nil {
		t.Fatal(err)
	}
	mf.lim.Annotate(mf.ctx, &view)
	for _, fam := range view.Families {
		for _, row := range fam.Classes {
			if fam.Family == FamilyHygiene && row.Class == ClassVacuum &&
				(row.Effective == nil || *row.Effective != L2 || row.MajorCap == nil) {
				t.Fatalf("autonomy view vacuum = %+v", row)
			}
		}
	}
}
