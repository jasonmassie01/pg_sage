package fleet

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// memLedger is Ledger in memory, with the store's state rules: rows move
// only forward, and Carried is each member's newest row when it waits
// (triaged, deferred to the period or before).
type memLedger struct {
	mu       sync.Mutex
	passes   []memPass
	rows     map[int64]map[string]*memRow
	startErr error
}

type memPass struct {
	id      int64
	period  string
	members int
	summary *PassSummary
}

type memRow struct {
	state     State
	outlier   Outlier
	deferred  string
	scheduled string
	ref       int64
	tokens    int
	runID     int64
}

func newMemLedger() *memLedger { return &memLedger{rows: map[int64]map[string]*memRow{}} }

func (l *memLedger) StartPass(_ context.Context, period string, members int) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.startErr != nil {
		return 0, l.startErr
	}
	id := int64(len(l.passes) + 1)
	l.passes = append(l.passes, memPass{id: id, period: period, members: members})
	l.rows[id] = map[string]*memRow{}
	return id, nil
}

func (l *memLedger) SaveRollups(_ context.Context, pass int64, rs []Rollup) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range rs {
		l.rows[pass][r.Database] = &memRow{state: StateRollup}
	}
	return nil
}

func (l *memLedger) row(pass int64, db string, want State) (*memRow, error) {
	r := l.rows[pass][db]
	if r == nil || r.state != want {
		return nil, fmt.Errorf("%w: %s in pass %d is not %s", ErrState, db, pass, want)
	}
	return r, nil
}

func (l *memLedger) MarkTriaged(_ context.Context, pass int64, os []Outlier) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, o := range os {
		r, err := l.row(pass, o.Database, StateRollup)
		if err != nil {
			return err
		}
		r.state, r.outlier = StateTriaged, o
	}
	return nil
}

func (l *memLedger) ScheduledIn(_ context.Context, period string) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, rows := range l.rows {
		for db, r := range rows {
			if r.scheduled == period && (r.state == StateDeepScheduled ||
				r.state == StateReviewed) {
				out = append(out, db)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func (l *memLedger) Carried(_ context.Context, period string) ([]Outlier, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	newest := map[string]int64{}
	for pass, rows := range l.rows {
		for db := range rows {
			if pass > newest[db] {
				newest[db] = pass
			}
		}
	}
	var out []Outlier
	for db, pass := range newest {
		r := l.rows[pass][db]
		if r.state == StateTriaged && r.deferred != "" && r.deferred <= period {
			o := r.outlier
			o.PassID, o.Carried = pass, true
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Database < out[j].Database })
	return out, nil
}

func (l *memLedger) MarkScheduled(_ context.Context, d Deep, ref int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, err := l.row(d.PassID, d.Database, StateTriaged)
	if err != nil {
		return err
	}
	r.state, r.ref, r.tokens, r.scheduled, r.deferred = StateDeepScheduled, ref, d.Tokens,
		d.Period, ""
	return nil
}

func (l *memLedger) MarkDeferred(_ context.Context, pass int64, db, to string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, err := l.row(pass, db, StateTriaged)
	if err != nil {
		return err
	}
	r.deferred = to
	return nil
}

func (l *memLedger) AwaitingReview(_ context.Context) ([]Scheduled, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Scheduled
	for pass, rows := range l.rows {
		for db, r := range rows {
			if r.state == StateDeepScheduled {
				out = append(out, Scheduled{Deep: Deep{Database: db, PassID: pass,
					Tokens: r.tokens, Period: r.scheduled}, Ref: r.ref})
			}
		}
	}
	return out, nil
}

func (l *memLedger) MarkReviewed(_ context.Context, pass int64, db string, run int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, err := l.row(pass, db, StateDeepScheduled)
	if err != nil {
		return err
	}
	r.state, r.runID = StateReviewed, run
	return nil
}

func (l *memLedger) FinishPass(_ context.Context, pass int64, s PassSummary) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.passes[pass-1].summary = &s
	return nil
}

func (l *memLedger) LastPass(_ context.Context, period string) (string, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.passes) - 1; i >= 0; i-- {
		if p := l.passes[i]; p.period == period && p.summary != nil {
			return p.summary.Status, true, nil
		}
	}
	return "", false, nil
}

// states counts the rows of pass by state.
func (l *memLedger) states(pass int64) map[State]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[State]int{}
	for _, r := range l.rows[pass] {
		out[r.state]++
	}
	return out
}

func (l *memLedger) get(pass int64, db string) memRow {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r := l.rows[pass][db]; r != nil {
		return *r
	}
	return memRow{}
}
