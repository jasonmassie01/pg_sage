package tuner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/planfresh"
)

// tuning-08 review: dropping a stale cached plan must not open the deferral
// gate. A statement reads the same tables after an ANALYZE, so a candidate
// whose only plan is stale still waits for the pending index
// recommendation on its table (fail closed).

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) log(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(format, args...))
}

func (l *logLines) has(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func deferCandidate(qid int64, s string) candidate {
	return candidate{QueryID: qid, Query: "SELECT * FROM " + s + ".t WHERE v = $1",
		Calls: 500, MeanExecTime: 500}
}

func TestTuneCandidates_StalePlanStillDefers(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	qid := -time.Now().UnixNano()
	s := planTable(t, pool, qid)
	cachePlan(t, pool, qid, "30 hours", diskSortOn(s))
	logs := &logLines{}
	tu := New(pool, TunerConfig{}, nil, logs.log)
	_, err := tu.tuneCandidates(ctx, []candidate{deferCandidate(qid, s)},
		map[string]bool{s + ".t": true})
	if err != nil {
		t.Fatalf("tuneCandidates: %v", err)
	}
	if !logs.has(fmt.Sprintf("deferring queryid=%d", qid)) {
		t.Fatalf("a stale plan on a table with a pending index recommendation must still "+
			"defer the candidate: %v", logs.lines)
	}
	if syms := tu.scanPlanForQuery(ctx, qid); len(syms) != 0 {
		t.Fatalf("the stale plan must never yield symptoms: %v", syms)
	}
}

func TestTuneCandidates_FailedFallbackStillDefers(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	qid := -time.Now().UnixNano()
	s := planTable(t, pool, qid)
	cachePlan(t, pool, qid, "30 hours", diskSortOn(s))
	logs := &logLines{}
	var calls int
	tu := New(pool, TunerConfig{}, nil, logs.log).WithPlanSource(
		func(context.Context, string) (planfresh.Plan, error) {
			calls++
			return planfresh.Plan{}, errors.New("permission denied for table t")
		}, catalogread.Default())
	_, err := tu.tuneCandidates(ctx, []candidate{deferCandidate(qid, s)},
		map[string]bool{s + ".t": true})
	if err != nil {
		t.Fatalf("tuneCandidates: %v", err)
	}
	if calls != 1 || !logs.has("cannot plan queryid") ||
		!logs.has(fmt.Sprintf("deferring queryid=%d", qid)) {
		t.Fatalf("a failed fallback EXPLAIN is logged and the stale relations still defer "+
			"(calls %d): %v", calls, logs.lines)
	}
}

func TestTuneCandidates_StalePlanOnOtherTableDoesNotDefer(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	qid := -time.Now().UnixNano()
	s := planTable(t, pool, qid)
	cachePlan(t, pool, qid, "30 hours", diskSortOn(s))
	logs := &logLines{}
	tu := New(pool, TunerConfig{}, nil, logs.log)
	if _, err := tu.tuneCandidates(ctx, []candidate{deferCandidate(qid, s)},
		map[string]bool{"other.table": true}); err != nil {
		t.Fatalf("tuneCandidates: %v", err)
	}
	if logs.has("deferring") {
		t.Fatalf("only a pending recommendation on a table the plan reads defers: %v",
			logs.lines)
	}
}
