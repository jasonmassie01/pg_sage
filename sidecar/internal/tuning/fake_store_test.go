package tuning

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// ---- fake store ----

type fakeStore struct {
	mu        sync.Mutex
	open      []analyzer.Finding
	openErr   error
	rejected  map[string]bool
	rejErr    error
	outcomes  []OutcomeSample
	outErr    error
	outCalls  int
	plans     map[int64]Plan
	extStats  []ExtStat
	colStats  []ColumnStat
	openCalls int
	catalog   *CatalogState // nil: every table and index exists, no index defs
	catErr    error
	actions   []SettingAction
	actErr    error
	queue     []string // in-flight index DDLs of the action queue
	queueErr  error
	day       map[string][2]int64 // UTC day -> tokens, requests charged
	dayErr    error
	chargeErr error
}

func (s *fakeStore) InFlightIndexes(context.Context) ([]string, error) {
	return s.queue, s.queueErr
}

func (s *fakeStore) DayBudgetUsed(_ context.Context, day time.Time) (int64, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dayErr != nil {
		return 0, 0, s.dayErr
	}
	u := s.day[day.UTC().Format(time.DateOnly)]
	return u[0], u[1], nil
}

func (s *fakeStore) ChargeDayBudget(_ context.Context, day time.Time, tokens,
	requests int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chargeErr != nil {
		return s.chargeErr
	}
	if s.day == nil {
		s.day = map[string][2]int64{}
	}
	k := day.UTC().Format(time.DateOnly)
	u := s.day[k]
	s.day[k] = [2]int64{u[0] + tokens, u[1] + requests}
	return nil
}

func (s *fakeStore) SettingActions(_ context.Context, since time.Time) (
	[]SettingAction, error) {
	if s.actErr != nil {
		return nil, s.actErr
	}
	var out []SettingAction
	for _, a := range s.actions {
		if !a.ExecutedAt.Before(since) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *fakeStore) Relations(_ context.Context, tables, indexes []string) (
	CatalogState, error) {
	if s.catErr != nil {
		return CatalogState{}, s.catErr
	}
	if s.catalog != nil {
		return *s.catalog, nil
	}
	st := CatalogState{Tables: map[string]bool{}, Indexes: map[string]bool{}}
	for _, t := range tables {
		st.Tables[t] = true
	}
	for _, i := range indexes {
		st.Indexes[i] = true
	}
	return st, nil
}

func (s *fakeStore) OpenFindings(_ context.Context, cats []string) ([]analyzer.Finding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.openCalls++
	return s.open, s.openErr
}

func (s *fakeStore) OperatorRejected(context.Context, time.Time) (map[string]bool, error) {
	return s.rejected, s.rejErr
}

func (s *fakeStore) Outcomes(_ context.Context, classes []string, _ time.Time, _ int) (
	[]OutcomeSample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outCalls++
	if s.outErr != nil {
		return nil, s.outErr
	}
	var out []OutcomeSample
	for _, o := range s.outcomes {
		for _, c := range classes {
			if o.Class == c {
				out = append(out, o)
			}
		}
	}
	return out, nil
}

func (s *fakeStore) Plan(_ context.Context, id int64, _ string) (Plan, error) {
	if p, ok := s.plans[id]; ok {
		return p, nil
	}
	return Plan{Source: PlanSourceNone}, nil
}

func (s *fakeStore) ExtendedStats(context.Context, string, string) ([]ExtStat, error) {
	return s.extStats, nil
}

func (s *fakeStore) ColumnStats(context.Context, string, string, []string) (
	[]ColumnStat, error) {
	return s.colStats, nil
}

var errFake = errors.New("fake failure")

// improvedOutcomes are n decided outcomes of a class/method predicted at
// predicted%, hits of which improved.
func improvedOutcomes(class, method string, predicted float64, n, hits int) []OutcomeSample {
	out := make([]OutcomeSample, 0, n)
	for i := 0; i < n; i++ {
		verdict := "neutral"
		if i < hits {
			verdict = "improved"
		}
		out = append(out, OutcomeSample{Class: class, Method: method,
			PredictedPct: predicted, Verdict: verdict, Tolerance: "met"})
	}
	return out
}
