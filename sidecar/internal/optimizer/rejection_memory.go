package optimizer

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// Rejection memory (lifeos 2026-10-03: one ai_claims index proposed 18
// times in three hours, each measured at 0.0%). A HypoPG what-if rejection
// of an LLM candidate is remembered with the workload it was measured on.
// While that workload and the table are materially unchanged, a candidate
// with the same shape skips the what-if, and the model is told the shape
// was already measured. Memory applies only to new LLM candidates: the
// re-evaluation of an open recommendation is never suppressed, and
// deterministic detectors do not pass through the optimizer at all.

const (
	maxRejectionsPerTable  = 100
	maxRejectionPromptLine = 300
	maxRejectionReason     = 1024
	maxRejectionDDL        = 8192
	minMeanTimeMs          = 0.001
)

// workloadSample is one target query as the what-if measured it.
type workloadSample struct {
	QueryID int64   `json:"queryid"`
	Calls   int64   `json:"calls"`
	MeanMs  float64 `json:"mean_ms"`
}

// rejection is one remembered what-if rejection.
type rejection struct {
	Schema, Table     string
	Shape             candidateShape
	DDL               string
	ImprovementPct    float64
	MinImprovementPct float64
	Reason            string
	Workload          []workloadSample
	RowEstimate       int64
	MeasuredAt        time.Time
	MeasureCount      int
}

// rejectionStore persists rejections (sage.optimizer_rejection).
type rejectionStore interface {
	recent(ctx context.Context, schema, table string, maxAge time.Duration,
		limit int) ([]rejection, error)
	record(ctx context.Context, r rejection) error
}

// memorySettings are the resolved thresholds; see
// config.OptimizerRejectionMemoryConfig.
type memorySettings struct {
	MaxAge                         time.Duration
	CallRatio, MeanRatio, RowRatio float64
	PromptMax                      int
	SkipLLMAfter                   int
}

// memorySettingsFrom resolves the configuration; an unset or invalid value
// (configuration validation refuses those) falls back to its default.
func memorySettingsFrom(c config.OptimizerRejectionMemoryConfig) memorySettings {
	days := config.DefaultOptRejectionMaxAgeDays
	if c.MaxAgeDays > 0 {
		days = min(c.MaxAgeDays, config.MaxOptRejectionMaxAgeDays)
	}
	prompt := config.DefaultOptRejectionPromptMaxShapes
	if c.PromptMaxShapes > 0 {
		prompt = min(c.PromptMaxShapes, config.MaxOptRejectionPromptMaxShapes)
	}
	skipAfter := config.DefaultOptRejectionSkipLLMAfter
	if c.SkipLLMAfter > 0 {
		skipAfter = min(c.SkipLLMAfter, config.MaxOptRejectionSkipLLMAfter)
	}
	return memorySettings{
		MaxAge:       time.Duration(days) * 24 * time.Hour,
		CallRatio:    ratioOr(c.CallVolumeRatio, config.DefaultOptRejectionCallVolumeRatio),
		MeanRatio:    ratioOr(c.MeanTimeRatio, config.DefaultOptRejectionMeanTimeRatio),
		RowRatio:     ratioOr(c.RowEstimateRatio, config.DefaultOptRejectionRowEstimateRatio),
		PromptMax:    prompt,
		SkipLLMAfter: skipAfter,
	}
}

func ratioOr(v, def float64) float64 {
	if v > 1 && v <= config.MaxOptRejectionRatio {
		return v
	}
	return def
}

// materialChange says why a rejection no longer describes the table ("" if
// it still does): it is older than the max age, the row estimate or a
// target query's calls or mean time moved by the configured factor, or the
// set of target queries changed.
func materialChange(r rejection, tc TableContext, s memorySettings, now time.Time) string {
	if age := now.Sub(r.MeasuredAt); age >= s.MaxAge {
		return fmt.Sprintf("measured %s ago, older than %s", age.Round(time.Minute), s.MaxAge)
	}
	if f := changeFactor(float64(r.RowEstimate), float64(tc.LiveTuples), 1); f >= s.RowRatio {
		return fmt.Sprintf("row estimate %d -> %d (%.1fx)", r.RowEstimate, tc.LiveTuples, f)
	}
	return workloadChange(r.Workload, workloadOf(tc), s)
}

func workloadChange(was, now []workloadSample, s memorySettings) string {
	prev := make(map[int64]workloadSample, len(was))
	for _, w := range was {
		if w.QueryID != 0 {
			prev[w.QueryID] = w
		}
	}
	seen := make(map[int64]bool, len(now))
	for _, w := range now {
		seen[w.QueryID] = true
		p, ok := prev[w.QueryID]
		if !ok {
			return fmt.Sprintf("new target query %d", w.QueryID)
		}
		if f := changeFactor(float64(p.Calls), float64(w.Calls), 1); f >= s.CallRatio {
			return fmt.Sprintf("calls of query %d changed %.1fx", w.QueryID, f)
		}
		if f := changeFactor(p.MeanMs, w.MeanMs, minMeanTimeMs); f >= s.MeanRatio {
			return fmt.Sprintf("mean time of query %d changed %.1fx", w.QueryID, f)
		}
	}
	for id := range prev {
		if !seen[id] {
			return fmt.Sprintf("target query %d no longer runs", id)
		}
	}
	return ""
}

// changeFactor is how many times larger the larger of a and b is, each
// raised to floor first (so a zero is a large change, not a division by
// zero).
func changeFactor(a, b, floor float64) float64 {
	a, b = max(a, floor), max(b, floor)
	return max(a, b) / min(a, b)
}

// workloadOf is the table's target queries as a what-if measures them.
func workloadOf(tc TableContext) []workloadSample {
	out := make([]workloadSample, 0, len(tc.Queries))
	for _, q := range tc.Queries {
		if q.QueryID != 0 {
			out = append(out, workloadSample{QueryID: q.QueryID, Calls: q.Calls,
				MeanMs: q.MeanTimeMs})
		}
	}
	return out
}

// rejectionMemory reads and writes remembered rejections. A nil memory is
// inert (memory disabled or no database).
type rejectionMemory struct {
	store    rejectionStore
	settings memorySettings
	now      func() time.Time
	logFn    func(string, string, ...any)
	mu       sync.Mutex
	streaks  map[string]*tableStreak // by "schema.table"
}

func newRejectionMemory(store rejectionStore, s memorySettings,
	logFn func(string, string, ...any)) *rejectionMemory {
	return &rejectionMemory{store: store, settings: s, now: time.Now, logFn: logFn,
		streaks: make(map[string]*tableStreak)}
}

// view loads the table's rejections that still describe it. A load
// failure is logged and leaves the view empty: every candidate is then
// measured (memory saves work; it never decides).
func (m *rejectionMemory) view(ctx context.Context, tc TableContext) *tableMemory {
	v := &tableMemory{}
	if m == nil {
		return v
	}
	v.mem = m
	rows, err := m.store.recent(ctx, tc.Schema, tc.Table, m.settings.MaxAge,
		maxRejectionsPerTable)
	if err != nil {
		m.logFn("WARN", "optimizer: rejection memory unavailable for %s.%s, "+
			"measuring every candidate: %v", tc.Schema, tc.Table, err)
		return v
	}
	now := m.now()
	for _, r := range rows {
		if materialChange(r, tc, m.settings, now) == "" {
			v.live = append(v.live, r)
		}
	}
	sort.SliceStable(v.live, func(i, j int) bool {
		return v.live[i].MeasuredAt.After(v.live[j].MeasuredAt)
	})
	return v
}

// remember records a complete what-if rejection of rec on tc. It reports
// whether the rejection was stored; a candidate it cannot normalize, or an
// oversized DDL, is not remembered.
func (m *rejectionMemory) remember(ctx context.Context, tc TableContext,
	rec Recommendation, minPct float64) (rejection, bool) {
	if m == nil {
		return rejection{}, false
	}
	table := tc.Schema + "." + tc.Table
	if len(rec.DDL) > maxRejectionDDL {
		m.logFn("DEBUG", "optimizer: rejection memory skips a %d-byte DDL on %s",
			len(rec.DDL), table)
		return rejection{}, false
	}
	shape, err := shapeOfCandidate(rec.DDL)
	if err != nil {
		m.logFn("DEBUG", "optimizer: rejection memory cannot normalize %s on %s: %v",
			rec.DDL, table, err)
		return rejection{}, false
	}
	r := rejection{Schema: tc.Schema, Table: tc.Table, Shape: shape, DDL: rec.DDL,
		ImprovementPct: rec.EstimatedImprovementPct, MinImprovementPct: minPct,
		Reason: truncateRunes(rec.WhatIfReason, maxRejectionReason), Workload: workloadOf(tc),
		RowEstimate: tc.LiveTuples, MeasuredAt: m.now(), MeasureCount: 1}
	if err := m.store.record(ctx, r); err != nil {
		m.logFn("WARN", "optimizer: remember what-if rejection of %s on %s: %v",
			rec.DDL, table, err)
		return r, false
	}
	return r, true
}

// tableMemory is one cycle's view of a table's remembered rejections.
type tableMemory struct {
	mem      *rejectionMemory
	live     []rejection // newest first
	skipped  int
	rejected int // what-if rejections this cycle
}

// suppress reports the remembered rejection of the same idea as rec, if
// any, and counts the skip. A nil view or an unparseable candidate never
// matches.
func (v *tableMemory) suppress(rec Recommendation) (rejection, bool) {
	if v == nil || len(v.live) == 0 {
		return rejection{}, false
	}
	shape, err := shapeOfCandidate(rec.DDL)
	if err != nil {
		return rejection{}, false
	}
	for _, r := range v.live {
		if r.Shape.sameIdea(shape) {
			v.skipped++
			return r, true
		}
	}
	return rejection{}, false
}

// learn adds a rejection measured this cycle, so a second copy of the idea
// in the same reply is skipped too.
func (v *tableMemory) learn(r rejection) {
	if v == nil || v.mem == nil {
		return
	}
	v.live = append([]rejection{r}, v.live...)
}

// promptLines lists the newest rejected shapes for the optimizer prompt,
// bounded in count and length.
func (v *tableMemory) promptLines() []string {
	if v == nil || v.mem == nil || len(v.live) == 0 {
		return nil
	}
	n := min(len(v.live), v.mem.settings.PromptMax)
	lines := make([]string, 0, n)
	for _, r := range v.live[:n] {
		lines = append(lines, promptLine(r))
	}
	return lines
}

func promptLine(r rejection) string {
	suffix := fmt.Sprintf(": %.1f%% call-weighted improvement, below the %.1f%% minimum "+
		"(measured %dx, last %s)", r.ImprovementPct, r.MinImprovementPct,
		max(r.MeasureCount, 1), r.MeasuredAt.UTC().Format("2006-01-02 15:04 UTC"))
	shape := llm.SanitizeForLLM(r.Shape.String())
	if budget := maxRejectionPromptLine - len("- ") - len(suffix); len(shape) > budget {
		shape = truncateBytes(shape, budget-len("...")) + "..."
	}
	return "- " + shape + suffix
}

// truncateBytes cuts s to at most n bytes on a rune boundary.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:max(n, 0)]
}

// truncateRunes cuts s to at most n runes.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// countRejection counts a what-if rejection of this cycle's reply.
func (v *tableMemory) countRejection() {
	if v != nil {
		v.rejected++
	}
}

// finishProposal feeds the reply's outcome to the table's streak.
func (v *tableMemory) finishProposal(tc TableContext, candidates int) {
	if v == nil || v.mem == nil {
		return
	}
	v.mem.noteProposal(tc, candidates, v.skipped+v.rejected)
}
