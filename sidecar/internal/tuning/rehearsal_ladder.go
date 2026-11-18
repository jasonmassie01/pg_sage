package tuning

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// A rehearsal is tier 2 of the verification ladder (lab-03a): its result
// becomes one LadderRecord with the clone receipt, and the rehearsals one
// database starts are capped per hour (clone.rehearsals_per_hour).

// DefaultRehearsalsPerHour is clone.rehearsals_per_hour's default.
const DefaultRehearsalsPerHour = 2

// ErrRehearsalCapped is a rehearsal skipped by the per-hour cap.
var ErrRehearsalCapped = errors.New("tuning: the clone rehearsal cap is reached")

// NoCloneProvider is the tier-2 skip reason when no clone provider is
// configured.
const NoCloneProvider = "no clone provider"

type hourlyLimit struct {
	inner   Rehearser
	perHour int
	now     func() time.Time

	mu      sync.Mutex
	started []time.Time
}

// LimitRehearsals caps inner at perHour rehearsals in any hour (0 or less:
// DefaultRehearsalsPerHour); now nil is time.Now. A request refused before
// any clone (not one index, no statement) does not use the cap.
func LimitRehearsals(inner Rehearser, perHour int, now func() time.Time) Rehearser {
	if perHour <= 0 {
		perHour = DefaultRehearsalsPerHour
	}
	if now == nil {
		now = time.Now
	}
	return &hourlyLimit{inner: inner, perHour: perHour, now: now}
}

func (l *hourlyLimit) Rehearse(ctx context.Context, req RehearsalRequest) (
	RehearsalResult, error) {
	at, ok := l.reserve()
	if !ok {
		return RehearsalResult{}, fmt.Errorf("%w: %d rehearsals in the last hour, the cap is "+
			"%d per hour (clone.rehearsals_per_hour)", ErrRehearsalCapped, l.perHour, l.perHour)
	}
	res, err := l.inner.Rehearse(ctx, req)
	if errors.Is(err, ErrRehearsalRefused) || errors.Is(err, ErrNoStatements) {
		l.release(at)
	}
	return res, err
}

func (l *hourlyLimit) reserve() (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	kept := l.started[:0]
	for _, t := range l.started {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	l.started = kept
	if len(l.started) >= l.perHour {
		return time.Time{}, false
	}
	l.started = append(l.started, now)
	return now, true
}

func (l *hourlyLimit) release(at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, t := range l.started {
		if t.Equal(at) {
			l.started = append(l.started[:i], l.started[i+1:]...)
			return
		}
	}
}

// RehearsalLadder is the tier-2 record of a rehearsal. An error is a
// skipped tier with the error (a clone that could not be made, the cap);
// otherwise the record passes when the executed statements' shared
// blocks per call fell, fails when they did not, and is inconclusive when
// no statement was executed before and after.
func RehearsalLadder(res RehearsalResult, err error, at time.Time) verify.LadderRecord {
	if err != nil {
		rec := verify.LadderBuildError(verify.TierClone, verify.MethodLadderRehearsal, err, at)
		if res.ReceiptID > 0 {
			rec.Evidence = map[string]any{"receipt_id": res.ReceiptID}
		}
		return rec
	}
	reh := verify.Rehearsal{ReceiptID: res.ReceiptID, Provider: res.Provider}
	before, after, executed := blockTotals(res.Queries)
	if executed == 0 {
		reh.Inconclusive = true
		reh.Reason = "no target statement was executed on the clone before and after: " +
			firstNotExecuted(res.Queries)
		return withCounts(verify.FromRehearsal(reh, at), executed, len(res.Queries))
	}
	pct := 0.0
	if before > 0 {
		pct = float64(after-before) / float64(before) * 100
	}
	reh.BlocksPerCallPct = &pct
	reh.Passed = after < before
	reh.Reason = fmt.Sprintf("shared blocks per call %d before, %d after on the clone",
		before, after)
	return withCounts(verify.FromRehearsal(reh, at), executed, len(res.Queries))
}

// blockTotals sums the blocks of the statements executed before and
// after.
func blockTotals(qs []RehearsedQuery) (before, after int64, executed int) {
	for _, q := range qs {
		if q.Error != "" || q.BeforeBlocks == nil || q.AfterBlocks == nil {
			continue
		}
		before += *q.BeforeBlocks
		after += *q.AfterBlocks
		executed++
	}
	return before, after, executed
}

func firstNotExecuted(qs []RehearsedQuery) string {
	for _, q := range qs {
		switch {
		case q.Error != "":
			return q.Error
		case q.NotExecuted != "":
			return q.NotExecuted
		}
	}
	return "no statement"
}

func withCounts(rec verify.LadderRecord, executed, total int) verify.LadderRecord {
	if rec.Evidence == nil {
		rec.Evidence = map[string]any{}
	}
	rec.Evidence["executed"] = executed
	rec.Evidence["statements"] = total
	return rec
}
