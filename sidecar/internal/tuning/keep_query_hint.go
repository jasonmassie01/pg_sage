package tuning

import (
	"context"
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
)

// An open query_tuning finding (the query tuner's hint, or the agent's)
// resolves on deterministic evidence only (tuning-06): its hint left the
// proposed state, its statement has been absent from pg_stat_statements for
// N cycles, or it has stayed below the query tuner's admission thresholds
// for N cycles. The counts live on the finding itself, so they survive a
// restart and only the agent advances them. Anything unknown (unreadable
// statistics, a queryid not reported, an interval the snapshots cannot
// measure) leaves the finding exactly as it was.

const (
	categoryQueryTuning = "query_tuning"
	detailAbsentCycles  = "absent_cycles"
	detailQuietCycles   = "quiet_cycles"
	// defaultHintAbsentCycles is tuner.hint_absent_cycles when unset.
	defaultHintAbsentCycles = 3
	// tunerSlowMeanMs is the query tuner's mean execution time bound: it
	// admits a statement whose mean is above it.
	tunerSlowMeanMs = 100.0
)

// hintAbsentCycles is N: tuner.hint_absent_cycles, 3 when zero or less.
func (a *Agent) hintAbsentCycles() int {
	if n := a.settings.HintAbsentCycles; n > 0 {
		return n
	}
	return defaultHintAbsentCycles
}

// queryTuningID is the queryid of a query_tuning finding ("queryid:N").
func queryTuningID(f analyzer.Finding) (int64, bool) {
	if f.Category != categoryQueryTuning {
		return 0, false
	}
	s, ok := strings.CutPrefix(f.ObjectIdentifier, "queryid:")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil && id != 0
}

// queryStates reads the statement state of every open query_tuning
// finding in one bounded read; nil when there is none or it is unreadable.
func (a *Agent) queryStates(ctx context.Context, open []analyzer.Finding) map[int64]QueryState {
	var ids []int64
	seen := map[int64]bool{}
	for _, f := range open {
		if id, ok := queryTuningID(f); ok && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	st, err := a.deps.Store.QueryStates(ctx, ids, a.settings.HintAdmission)
	if err != nil {
		a.logFn("WARN", "tuning: statement statistics unreadable, every open query_tuning "+
			"finding is kept: %v", err)
		return nil
	}
	return st
}

// intervalQuiet judges each statement both snapshots hold against the
// query tuner's thresholds over the interval: true when it did not run, or
// ran at a mean of at most 100 ms without writing temp blocks. A statement
// the interval cannot measure (missing from a snapshot, a statistics
// reset) is absent: unknown.
func intervalQuiet(cur, prev *collector.Snapshot) map[int64]bool {
	if cur == nil {
		return nil
	}
	before := priorCounters(cur, prev)
	if before == nil {
		return nil
	}
	out := map[int64]bool{}
	for _, q := range cur.Queries {
		if _, ok := before[q.QueryID]; !ok {
			continue
		}
		if s := delta(q, before); s.Windowed {
			out[q.QueryID] = s.Calls == 0 ||
				s.MeanMs <= tunerSlowMeanMs && s.TempBlksWritten == 0
		}
	}
	return out
}

// keepQueryHint advances an open query_tuning finding's counts by this
// cycle's evidence and returns why it resolves ("" keeps it).
func (a *Agent) keepQueryHint(f analyzer.Finding, states map[int64]QueryState,
	quiet map[int64]bool) (analyzer.Finding, string) {
	id, ok := queryTuningID(f)
	if !ok {
		return f, ""
	}
	st, known := states[id]
	if !known {
		return f, ""
	}
	// Only a finding with installable SQL recorded its hint in query_hints;
	// an operator-hint or advisory finding must not resolve on an older,
	// unrelated row (the tuner would re-open it after its cooldown).
	if f.RecommendedSQL != "" && st.HintStatus != "" && st.HintStatus != "proposed" {
		return f, "its hint is " + st.HintStatus
	}
	n := a.hintAbsentCycles()
	absent, _ := cycleCount(f.Detail[detailAbsentCycles])
	still, _ := cycleCount(f.Detail[detailQuietCycles])
	if !st.Present {
		if absent+1 >= n {
			return f, fmt.Sprintf("absent from pg_stat_statements for %d cycles", n)
		}
		return withCycleCounts(f, absent+1, still), ""
	}
	isQuiet, measured := quiet[id]
	switch {
	case st.Admitted || measured && !isQuiet:
		still = 0
	case measured:
		if still+1 >= n {
			return f, fmt.Sprintf("below the query tuner's admission thresholds for %d "+
				"cycles", n)
		}
		still++
	}
	return withCycleCounts(f, 0, still), ""
}

// withCycleCounts is f with its counts set (a zero count is removed). The
// detail is copied when it changes: the open finding may be shared.
func withCycleCounts(f analyzer.Finding, absent, quiet int) analyzer.Finding {
	want := map[string]int{detailAbsentCycles: absent, detailQuietCycles: quiet}
	same := true
	for key, v := range want {
		cur, _ := cycleCount(f.Detail[key])
		_, has := f.Detail[key]
		same = same && cur == v && has == (v > 0)
	}
	if same {
		return f
	}
	f.Detail = maps.Clone(f.Detail)
	if f.Detail == nil {
		f.Detail = map[string]any{}
	}
	for key, v := range want {
		if v > 0 {
			f.Detail[key] = v
		} else {
			delete(f.Detail, key)
		}
	}
	return f
}

// cycleCount reads a count stored on a finding's detail (a JSON number
// once read back); anything else is no count.
func cycleCount(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		if n >= 0 {
			return n, true
		}
	case int64:
		if n >= 0 && n <= math.MaxInt32 {
			return int(n), true
		}
	case float64:
		if n >= 0 && n == math.Trunc(n) && n <= math.MaxInt32 {
			return int(n), true
		}
	}
	return 0, false
}
