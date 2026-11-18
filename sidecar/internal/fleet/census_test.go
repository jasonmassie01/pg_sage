package fleet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cost"
)

// fleetpol-03 unit tests: the census rules over a fake meta store and
// fake measurements. census_db_test.go runs them on real PostgreSQL.

var censusNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func tp(t time.Time) *time.Time { return &t }
func ip(v int64) *int64         { return &v }

const day = 24 * time.Hour

// fakeCensusStore records what a census run wrote. BeginCensus hands each
// period out once, as the unique period does.
type fakeCensusStore struct {
	mu        sync.Mutex
	subjects  []CensusSubject
	subjErr   error
	writeErr  error
	begun     map[time.Time]bool
	begins    int
	runs      []CensusRun
	rows      [][]CensusRow
	beginWait chan struct{}
}

func (f *fakeCensusStore) BeginCensus(_ context.Context, period, _ time.Time,
	_ string) (int64, bool, error) {
	if f.beginWait != nil {
		<-f.beginWait
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.begins++
	if f.begun == nil {
		f.begun = map[time.Time]bool{}
	}
	if f.begun[period] {
		return 0, false, nil
	}
	f.begun[period] = true
	return int64(len(f.begun)), true, nil
}

func (f *fakeCensusStore) CensusSubjects(context.Context) ([]CensusSubject, error) {
	return f.subjects, f.subjErr
}

func (f *fakeCensusStore) WriteCensus(_ context.Context, run CensusRun,
	rows []CensusRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	f.runs = append(f.runs, run)
	f.rows = append(f.rows, rows)
	return nil
}

// fixedRates prices only the databases it names.
type fixedRates map[string]cost.Rates

func (r fixedRates) Rates(_, database string) (cost.Rates, bool) {
	v, ok := r[database]
	return v, ok
}

func storageRate(perGB float64) cost.Rates {
	return cost.Rates{Source: cost.SourceConfig, Currency: "USD", StorageGBMonth: perGB,
		AsOf: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
}

func measures(m map[string]CensusMeasure, errs map[string]error) CensusMeasureFunc {
	return func(_ context.Context, s CensusSubject) (CensusMeasure, error) {
		if err := errs[s.Key]; err != nil {
			return CensusMeasure{}, err
		}
		got, ok := m[s.Key]
		if !ok {
			return CensusMeasure{}, fmt.Errorf("no fixture measure for %s", s.Key)
		}
		return got, nil
	}
}

func newTestCensus(st CensusStore, o CensusOptions) *Census {
	o.Now = func() time.Time { return censusNow }
	if o.Holder == "" {
		o.Holder = "sidecar-a"
	}
	return NewCensus(st, o)
}

func rowByKey(t *testing.T, rows []CensusRow, key string) CensusRow {
	t.Helper()
	for _, r := range rows {
		if r.DatabaseKey == key {
			return r
		}
	}
	t.Fatalf("no census row for %s in %+v", key, rows)
	return CensusRow{}
}

func happySubjects() []CensusSubject {
	return []CensusSubject{
		{Key: "agent-reused", Kind: CensusObserveOnly, Creator: "agent:claude",
			Owner: "data-team", CreatedAt: tp(censusNow.Add(-40 * day)), Discovered: true},
		{Key: "agent-idle", Kind: CensusObserveOnly, Creator: "agent:claude",
			Owner: "data-team", CreatedAt: tp(censusNow.Add(-60 * day)), Discovered: true},
		{Key: "person-db", Kind: CensusFull, Creator: "person:jo", Owner: "jo",
			CreatedAt: tp(censusNow.Add(-100 * day))},
		{Key: "agent-young", Kind: CensusFull, Creator: "agent:cursor",
			CreatedAt: tp(censusNow.Add(-3 * day)), Discovered: true},
		{Key: "neon/br-1", Kind: CensusDiscovered, Creator: "agent:claude", Owner: "data-team",
			CreatedAt: tp(censusNow.Add(-20 * day)), Discovered: true},
	}
}

func happyMeasures() map[string]CensusMeasure {
	return map[string]CensusMeasure{
		"agent-reused": {LastActivityAt: tp(censusNow.Add(-2 * day)),
			ObservedFrom: tp(censusNow.Add(-40 * day)), QuietFor: 2 * day,
			SizeBytes: ip(2 << 30), Basis: "fixture"},
		"agent-idle": {LastActivityAt: tp(censusNow.Add(-59 * day)),
			ObservedFrom: tp(censusNow.Add(-60 * day)), QuietFor: 59 * day,
			SizeBytes: ip(1 << 30), Basis: "fixture"},
		"person-db": {ObservedFrom: tp(censusNow.Add(-60 * day)), QuietFor: 50 * day,
			SizeBytes: ip(5 << 30), Basis: "fixture"},
		"agent-young": {LastActivityAt: tp(censusNow.Add(-1 * day)),
			ObservedFrom: tp(censusNow.Add(-3 * day)), QuietFor: day, Basis: "fixture"},
	}
}

func TestCensus_HappyPathRowsAndShare(t *testing.T) {
	st := &fakeCensusStore{subjects: happySubjects()}
	c := newTestCensus(st, CensusOptions{Measure: measures(happyMeasures(), nil),
		Rates: fixedRates{"agent-reused": storageRate(0.10)}})
	run, ok, err := c.RunOnce(context.Background())
	if err != nil || !ok {
		t.Fatalf("RunOnce = %v %v, want a run", ok, err)
	}
	if len(st.rows) != 1 || len(st.rows[0]) != 5 {
		t.Fatalf("wrote %d runs (%v), want one run of 5 rows", len(st.rows), st.rows)
	}
	rows := st.rows[0]
	reused := rowByKey(t, rows, "agent-reused")
	if reused.Status != CensusMeasured || reused.Reused == nil || !*reused.Reused ||
		reused.Proposal != "" || reused.CostMonth == nil ||
		fmt.Sprintf("%.2f", *reused.CostMonth) != "0.20" ||
		!strings.Contains(reused.CostNote, "USD per month at the operator's storage rate") ||
		reused.Owner != "data-team" || reused.Creator != "agent:claude" {
		t.Fatalf("agent-reused row %+v", reused)
	}
	idle := rowByKey(t, rows, "agent-idle")
	if idle.Reused == nil || *idle.Reused ||
		idle.Proposal != "no real activity in 59 days: propose TTL 7 days" ||
		idle.CostMonth != nil || !strings.HasPrefix(idle.CostNote, CensusUnpriced) {
		t.Fatalf("agent-idle row %+v", idle)
	}
	person := rowByKey(t, rows, "person-db")
	if person.Reused != nil || person.Proposal != "" || person.LastActivityAt != nil ||
		!strings.Contains(person.Note, "no real activity") {
		t.Fatalf("person-db row %+v: a person's database is listed, never proposed", person)
	}
	if young := rowByKey(t, rows, "agent-young"); young.Reused != nil {
		t.Fatalf("a 3-day-old database was judged for reuse: %+v", young)
	}
	never := rowByKey(t, rows, "neon/br-1")
	if never.Status != CensusNotEnrolled || never.Reused != nil || never.SizeBytes != nil ||
		never.Kind != CensusDiscovered {
		t.Fatalf("never-enrolled row %+v", never)
	}
	if run.Databases != 5 || run.AgentCreated != 4 || run.ReuseEligible != 2 ||
		run.Reused != 1 || run.Proposals != 1 || run.Unreachable != 0 ||
		run.ReuseShare == nil || *run.ReuseShare != 0.5 ||
		!run.Period.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) ||
		!run.RunAt.Equal(censusNow) {
		t.Fatalf("run %+v, want 5 databases, 4 agent-created, share 1/2, 1 proposal", run)
	}
	if st.runs[0].ID != run.ID || run.ID == 0 {
		t.Fatalf("written run id %d, returned %d", st.runs[0].ID, run.ID)
	}
}

func TestCensusRowFor_NegativeSizeIsStoredUnknownWithReason(t *testing.T) {
	s := CensusSubject{Key: "b", Kind: CensusFull}
	row := CensusRowFor(s, CensusMeasure{SizeBytes: ip(-4096), Basis: "provider"}, nil,
		fixedRates{"b": storageRate(0.1)}, censusNow)
	if row.SizeBytes != nil || !strings.Contains(row.SizeNote, "invalid size -4096") ||
		!strings.Contains(row.SizeNote, "provider") || row.CostMonth != nil ||
		!strings.Contains(row.CostNote, "size not measured") {
		t.Fatalf("row %+v, want size NULL with the reason and no cost", row)
	}
	zero := CensusRowFor(s, CensusMeasure{SizeBytes: ip(0)}, nil,
		fixedRates{"b": storageRate(0.1)}, censusNow)
	if zero.SizeBytes == nil || *zero.SizeBytes != 0 || zero.CostMonth == nil ||
		*zero.CostMonth != 0 {
		t.Fatalf("zero size row %+v, want size 0 priced at 0", zero)
	}
	none := CensusRowFor(s, CensusMeasure{}, nil, nil, censusNow)
	if none.SizeBytes != nil || none.SizeNote != "not measured" ||
		none.CostNote != "unpriced: no operator rates (cost.rates)" {
		t.Fatalf("unmeasured size row %+v", none)
	}
}

func TestCensusRowFor_UnpricedReasons(t *testing.T) {
	s := CensusSubject{Key: "c", Kind: CensusFull}
	m := CensusMeasure{SizeBytes: ip(1 << 30)}
	for _, tc := range []struct {
		rates cost.RateSource
		want  string
	}{
		{nil, "unpriced: no operator rates (cost.rates)"},
		{fixedRates{}, "unpriced: no cost.rates entry for c"},
		{fixedRates{"c": storageRate(0)}, "unpriced: the cost.rates entry for c has no " +
			"storage rate"},
	} {
		row := CensusRowFor(s, m, nil, tc.rates, censusNow)
		if row.CostMonth != nil || row.CostNote != tc.want {
			t.Errorf("cost %v %q, want unpriced %q", row.CostMonth, row.CostNote, tc.want)
		}
	}
}

func TestCensus_NoMembersWritesAnEmptyRun(t *testing.T) {
	st := &fakeCensusStore{}
	run, ok, err := newTestCensus(st, CensusOptions{Measure: measures(nil, nil)}).
		RunOnce(context.Background())
	if err != nil || !ok {
		t.Fatalf("RunOnce = %v %v", ok, err)
	}
	if len(st.runs) != 1 || len(st.rows[0]) != 0 || st.rows[0] == nil {
		t.Fatalf("runs %v rows %v, want one empty, non-nil run", st.runs, st.rows)
	}
	if run.Databases != 0 || run.ReuseShare != nil || run.ReuseEligible != 0 {
		t.Fatalf("empty run %+v, want zero counts and no share", run)
	}
}

func TestCensus_UnreachableMemberIsListedAndTheRunCompletes(t *testing.T) {
	st := &fakeCensusStore{subjects: []CensusSubject{
		{Key: "down", Kind: CensusObserveOnly, Discovered: true},
		{Key: "broken", Kind: CensusFull},
		{Key: "up", Kind: CensusFull},
	}}
	errs := map[string]error{
		"down":   fmt.Errorf("%w: down: dial tcp: refused", ErrObserveUnreachable),
		"broken": errors.New("read the workload calls: permission denied"),
	}
	m := map[string]CensusMeasure{"up": {QuietFor: day, ObservedFrom: tp(censusNow)}}
	run, ok, err := newTestCensus(st, CensusOptions{Measure: measures(m, errs)}).
		RunOnce(context.Background())
	if err != nil || !ok {
		t.Fatalf("RunOnce = %v %v, want the run to complete", ok, err)
	}
	rows := st.rows[0]
	down := rowByKey(t, rows, "down")
	if down.Status != CensusUnreachable || !strings.Contains(down.Note, "refused") ||
		down.Proposal != "" {
		t.Fatalf("down row %+v", down)
	}
	broken := rowByKey(t, rows, "broken")
	if broken.Status != CensusUnmeasured || !strings.Contains(broken.Note, "permission denied") {
		t.Fatalf("broken row %+v", broken)
	}
	if rowByKey(t, rows, "up").Status != CensusMeasured || run.Unreachable != 1 ||
		run.Databases != 3 {
		t.Fatalf("run %+v rows %+v", run, rows)
	}
}

func TestCensus_ErrorsPropagateWithContext(t *testing.T) {
	st := &fakeCensusStore{subjErr: errors.New("connection refused")}
	_, _, err := newTestCensus(st, CensusOptions{Measure: measures(nil, nil)}).
		RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "listing the census subjects") ||
		!strings.Contains(err.Error(), "connection refused") || len(st.runs) != 0 {
		t.Fatalf("err %v runs %v, want the listing error and no run written", err, st.runs)
	}
	st = &fakeCensusStore{writeErr: errors.New("disk full")}
	_, ok, err := newTestCensus(st, CensusOptions{Measure: measures(nil, nil)}).
		RunOnce(context.Background())
	if err == nil || ok || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("write failure: ok %v err %v", ok, err)
	}
}

func TestCensusRowFor_ReuseBoundaryIsSevenDays(t *testing.T) {
	created := censusNow.Add(-30 * day)
	s := CensusSubject{Key: "b", Kind: CensusObserveOnly, Creator: "agent:x", CreatedAt: &created}
	for _, tc := range []struct {
		last *time.Time
		want bool
	}{
		{tp(created.Add(7 * day)), true},
		{tp(created.Add(7*day - time.Second)), false},
		{nil, false},
	} {
		row := CensusRowFor(s, CensusMeasure{LastActivityAt: tc.last, ObservedFrom: &created},
			nil, nil, censusNow)
		if row.Reused == nil || *row.Reused != tc.want {
			t.Errorf("last activity %v: reused %v, want %v", tc.last, row.Reused, tc.want)
		}
	}
	// Observed only from day 9 with no activity: the census cannot say.
	late := created.Add(9 * day)
	row := CensusRowFor(s, CensusMeasure{ObservedFrom: &late}, nil, nil, censusNow)
	if row.Reused != nil {
		t.Fatalf("reused %v, want unknown when day 7 was not observed", *row.Reused)
	}
	// Created exactly seven days ago is old enough to judge.
	edge := censusNow.Add(-7 * day)
	s.CreatedAt = &edge
	r := CensusRowFor(s, CensusMeasure{ObservedFrom: &edge}, nil, nil, censusNow)
	if r.Reused == nil {
		t.Fatal("a database created exactly 7 days ago was not judged")
	}
	if r := CensusRowFor(CensusSubject{Key: "p", Creator: "person:a", CreatedAt: &created},
		CensusMeasure{LastActivityAt: &censusNow}, nil, nil, censusNow); r.Reused != nil {
		t.Fatal("a person's database counted in the agent reuse share")
	}
}

func TestCensusRowFor_ProposalBoundaryAndStates(t *testing.T) {
	s := CensusSubject{Key: "b", Kind: CensusObserveOnly, Discovered: true}
	at := func(q time.Duration, s CensusSubject) string {
		return CensusRowFor(s, CensusMeasure{QuietFor: q}, nil, nil, censusNow).Proposal
	}
	if got := at(30*day, s); got != "no real activity in 30 days: propose TTL 7 days" {
		t.Fatalf("30 quiet days: proposal %q", got)
	}
	if got := at(30*day-time.Second, s); got != "" {
		t.Fatalf("just under 30 days: proposal %q, want none", got)
	}
	// proposed -> owner adopts -> withdrawn: an adopted member is not proposed.
	adopted := s
	adopted.Adopted = true
	if got := at(90*day, adopted); got != "" {
		t.Fatalf("adopted member proposed %q", got)
	}
	if got := CensusRowFor(s, CensusMeasure{QuietFor: 90 * day, Unmeasured: "no reading"},
		nil, nil, censusNow).Proposal; got != "" {
		t.Fatalf("unmeasured member proposed %q", got)
	}
}

func TestCensus_NotLeaderOrTakenPeriodWritesNothing(t *testing.T) {
	st := &fakeCensusStore{}
	c := newTestCensus(st, CensusOptions{Measure: measures(nil, nil),
		IsLeader: func() bool { return false }})
	if _, ok, err := c.RunOnce(context.Background()); ok || err != nil || st.begins != 0 {
		t.Fatalf("follower: ok %v err %v begins %d, want nothing", ok, err, st.begins)
	}
	leader := newTestCensus(st, CensusOptions{Measure: measures(nil, nil)})
	if _, ok, _ := leader.RunOnce(context.Background()); !ok {
		t.Fatal("first run of the month did not run")
	}
	if _, ok, err := leader.RunOnce(context.Background()); ok || err != nil ||
		len(st.runs) != 1 {
		t.Fatalf("second run of the month: ok %v err %v runs %d, want 1", ok, err,
			len(st.runs))
	}
}

func TestCensus_ConcurrentRunsWriteOneRun(t *testing.T) {
	st := &fakeCensusStore{beginWait: make(chan struct{})}
	var wg sync.WaitGroup
	oks := make([]bool, 2)
	for i := range 2 {
		c := newTestCensus(st, CensusOptions{Holder: fmt.Sprintf("s%d", i),
			Measure: measures(nil, nil)})
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, oks[i], _ = c.RunOnce(context.Background())
		}()
	}
	close(st.beginWait)
	wg.Wait()
	if oks[0] == oks[1] || len(st.runs) != 1 {
		t.Fatalf("oks %v runs %d, want exactly one run", oks, len(st.runs))
	}
}

func TestCensusPeriod_IsTheUTCMonth(t *testing.T) {
	chicago := time.FixedZone("CST", -6*3600)
	for in, want := range map[time.Time]time.Time{
		time.Date(2026, 11, 30, 18, 0, 0, 0, chicago): time.Date(2026, 12, 1, 0, 0, 0, 0,
			time.UTC),
		time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC): time.Date(2026, 11, 1, 0, 0, 0, 0,
			time.UTC),
	} {
		if got := CensusPeriod(in); !got.Equal(want) {
			t.Errorf("CensusPeriod(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestNewCensus_NilStoreAndDefaults(t *testing.T) {
	if NewCensus(nil, CensusOptions{}) != nil {
		t.Fatal("a census without a store")
	}
	c := NewCensus(&fakeCensusStore{}, CensusOptions{})
	if c.opts.Concurrency != DefaultCensusConcurrency || c.opts.Every != DefaultCensusEvery ||
		c.opts.MemberTimeout != DefaultCensusMemberTimeout || c.opts.Measure == nil {
		t.Fatalf("defaults %+v", c.opts)
	}
	// The default measure without a measurer: every member is unmeasured.
	m, err := c.opts.Measure(context.Background(), CensusSubject{Key: "x"})
	if err != nil || m.Unmeasured == "" {
		t.Fatalf("default measure %+v %v, want unmeasured", m, err)
	}
}
