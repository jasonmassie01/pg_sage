package tuning

import (
	"fmt"
	"sort"

	"github.com/pg-sage/sidecar/internal/collector"
)

// Case detection (owner decision 1): a case is a workload problem — a top
// statement by call-weighted time, a regression, or write amplification —
// never a per-table prompt. Only workload statements and tables count.

// CaseKind is what kind of workload problem a case is.
type CaseKind string

// Case kinds, in the order they are examined.
const (
	CaseRegression         CaseKind = "regression"
	CaseTopStatement       CaseKind = "top_statement"
	CaseWriteAmplification CaseKind = "write_amplification"
)

var kindOrder = map[CaseKind]int{CaseRegression: 0, CaseTopStatement: 1,
	CaseWriteAmplification: 2}

// Case is one workload problem with its statements and tables.
type Case struct {
	ID         string
	Kind       CaseKind
	Weight     float64
	Statements []CaseStatement
	Tables     []string
	Reason     string
}

// CaseStatement is one statement of a case, over the interval.
type CaseStatement struct {
	QueryID         int64
	Text            string
	Class           StatementClass
	Calls           int64
	TotalMs         float64
	MeanMs          float64
	PrevMeanMs      float64
	TempBlksWritten int64
	SharedBlksRead  int64
	SharedBlksHit   int64
	Rows            int64
	Share           float64
}

// Thresholds decide what is a case. A zero field takes its default.
type Thresholds struct {
	MinShare             float64
	MinTotalMs           float64
	MinCalls             int64
	RegressionFactor     float64
	MinIndexWritesPerSec float64
	MinDeadTuples        int64
	MinDeadRatio         float64
	MinTableBytes        int64
	MaxCases             int
}

// DefaultThresholds are the shipped case thresholds.
func DefaultThresholds() Thresholds {
	return Thresholds{MinShare: 0.05, MinTotalMs: 1000, MinCalls: 10, RegressionFactor: 2,
		MinIndexWritesPerSec: 50, MinDeadTuples: 1000, MinDeadRatio: 0.2,
		MinTableBytes: 8 << 20, MaxCases: 20}
}

func (th Thresholds) withDefaults() Thresholds {
	d := DefaultThresholds()
	pick := func(v, def float64) float64 {
		if v > 0 {
			return v
		}
		return def
	}
	th.MinShare = pick(th.MinShare, d.MinShare)
	th.MinTotalMs = pick(th.MinTotalMs, d.MinTotalMs)
	th.RegressionFactor = pick(th.RegressionFactor, d.RegressionFactor)
	th.MinIndexWritesPerSec = pick(th.MinIndexWritesPerSec, d.MinIndexWritesPerSec)
	th.MinDeadRatio = pick(th.MinDeadRatio, d.MinDeadRatio)
	if th.MinCalls <= 0 {
		th.MinCalls = d.MinCalls
	}
	if th.MinDeadTuples <= 0 {
		th.MinDeadTuples = d.MinDeadTuples
	}
	if th.MinTableBytes <= 0 {
		th.MinTableBytes = d.MinTableBytes
	}
	if th.MaxCases <= 0 {
		th.MaxCases = d.MaxCases
	}
	return th
}

// DetectCases finds the cases of the interval prev..cur (cumulative
// counters when there is no usable previous snapshot): regressions, then
// top statements, then write amplification, each by weight, at most
// MaxCases.
func DetectCases(cur, prev *collector.Snapshot, w Workload, th Thresholds) []Case {
	if cur == nil {
		return nil
	}
	th = th.withDefaults()
	stmts := intervalStatements(cur, prev, w)
	cases := statementCases(stmts, th)
	cases = append(cases, writeCases(cur, prev, w, stmts, th)...)
	sort.SliceStable(cases, func(i, j int) bool {
		a, b := cases[i], cases[j]
		if kindOrder[a.Kind] != kindOrder[b.Kind] {
			return kindOrder[a.Kind] < kindOrder[b.Kind]
		}
		if a.Weight != b.Weight {
			return a.Weight > b.Weight
		}
		return a.ID < b.ID
	})
	if len(cases) > th.MaxCases {
		cases = cases[:th.MaxCases]
	}
	return cases
}

// intervalStmt is a workload statement over the interval, with its tables.
type intervalStmt struct {
	CaseStatement
	tables   []string
	hasPrior bool
}

// intervalStatements are the workload statements with interval deltas
// (cumulative counters on the first cycle and after a reset), each with
// its share of the workload's interval time.
func intervalStatements(cur, prev *collector.Snapshot, w Workload) []intervalStmt {
	before := priorCounters(cur, prev)
	var out []intervalStmt
	total := 0.0
	for _, q := range cur.Queries {
		if !w.IsWorkload(q.QueryID) {
			continue
		}
		s := delta(q, before)
		s.Class = w.Statements[q.QueryID].Class
		s.tables = w.Statements[q.QueryID].Tables
		total += s.TotalMs
		out = append(out, s)
	}
	for i := range out {
		if total > 0 {
			out[i].Share = out[i].TotalMs / total
		}
	}
	return out
}

// priorCounters are the previous snapshot's counters by queryid, or nil
// when the interval cannot be computed (no snapshot, a statistics reset).
func priorCounters(cur, prev *collector.Snapshot) map[int64]collector.QueryStats {
	if prev == nil || cur.StatsReset || (!cur.StatsEpoch.IsZero() &&
		!prev.StatsEpoch.IsZero() && !cur.StatsEpoch.Equal(prev.StatsEpoch)) {
		return nil
	}
	out := make(map[int64]collector.QueryStats, len(prev.Queries))
	for _, q := range prev.Queries {
		out[q.QueryID] = q
	}
	return out
}

// delta is q over the interval; a statement absent before, or whose
// counters went backwards, uses its cumulative counters.
func delta(q collector.QueryStats, before map[int64]collector.QueryStats) intervalStmt {
	s := intervalStmt{CaseStatement: CaseStatement{QueryID: q.QueryID, Text: q.Query,
		Calls: q.Calls, TotalMs: q.TotalExecTime, TempBlksWritten: q.TempBlksWritten,
		SharedBlksRead: q.SharedBlksRead, SharedBlksHit: q.SharedBlksHit, Rows: q.Rows}}
	p, ok := before[q.QueryID]
	if ok && q.Calls >= p.Calls && q.TotalExecTime >= p.TotalExecTime {
		s.Calls -= p.Calls
		s.TotalMs -= p.TotalExecTime
		s.TempBlksWritten = max(s.TempBlksWritten-p.TempBlksWritten, 0)
		s.SharedBlksRead = max(s.SharedBlksRead-p.SharedBlksRead, 0)
		s.SharedBlksHit = max(s.SharedBlksHit-p.SharedBlksHit, 0)
		s.Rows = max(s.Rows-p.Rows, 0)
		if p.Calls > 0 {
			s.PrevMeanMs = p.TotalExecTime / float64(p.Calls)
			s.hasPrior = true
		}
	}
	if s.Calls > 0 {
		s.MeanMs = s.TotalMs / float64(s.Calls)
	}
	return s
}

// statementCases are the regressions and top statements; one statement
// makes at most one case and a regression wins.
func statementCases(stmts []intervalStmt, th Thresholds) []Case {
	var out []Case
	for _, s := range stmts {
		if s.Calls < th.MinCalls || s.TotalMs < th.MinTotalMs {
			continue
		}
		c := Case{Weight: s.Share, Statements: []CaseStatement{s.CaseStatement},
			Tables: s.tables}
		switch {
		case s.hasPrior && s.PrevMeanMs > 0 && s.MeanMs >= th.RegressionFactor*s.PrevMeanMs:
			c.Kind, c.ID = CaseRegression, fmt.Sprintf("regression:%d", s.QueryID)
			c.Reason = fmt.Sprintf("mean time rose from %.2f ms to %.2f ms (%.1fx) over "+
				"%d calls", s.PrevMeanMs, s.MeanMs, s.MeanMs/s.PrevMeanMs, s.Calls)
		case s.Share >= th.MinShare:
			c.Kind, c.ID = CaseTopStatement, fmt.Sprintf("top_statement:%d", s.QueryID)
			c.Reason = fmt.Sprintf("%.1f%% of the workload's time (%.0f ms over %d calls)",
				s.Share*100, s.TotalMs, s.Calls)
		default:
			continue
		}
		out = append(out, c)
	}
	return out
}
