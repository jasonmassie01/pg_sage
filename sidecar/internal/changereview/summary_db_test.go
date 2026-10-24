package changereview

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Calibration (concurrence and false-safe per verdict class) and the
// Reviews-page summary are measured from sage.change_review only.

type seed struct {
	verdict, dba, receipt, credit string
	minutes                       int
	tokens                        int
	shadow                        *bool
	score                         string
	pr                            string
	findings                      []Finding
	age                           time.Duration
}

func seedReviews(t *testing.T, f *fixture, st *Store, seeds []seed) []Review {
	t.Helper()
	var out []Review
	for _, s := range seeds {
		s := s
		out = append(out, completeReview(t, f, st, func(r *Review) {
			now := utc(time.Now().Add(-s.age))
			r.Verdict, r.DeterministicVerdict = s.verdict, s.verdict
			r.CreatedAt, r.CompletedAt, r.UpdatedAt = now, ptr(now.Add(time.Minute)), now
			r.Tokens, r.ShadowAutoClear, r.PRURL, r.Findings = s.tokens, s.shadow, s.pr,
				s.findings
			if s.score != "" {
				r.ShadowScore = s.score
			}
			if s.dba != "" {
				r.DBAVerdict, r.DBAActor = s.dba, "user:1"
				r.DBADecidedAt = ptr(now.Add(time.Hour))
				if s.dba == DBAOverride {
					r.DBAReason, r.DBADirection = "reason "+s.verdict, DirectionTooStrict
				}
			}
			if s.receipt != "" && s.receipt != ReceiptPending {
				r.LandingState, r.LandingSource = LandingLanded, LandingSourceCatalog
				r.LandedLower, r.LandedAt = ptr(now), ptr(now)
				r.ReceiptDecidedAt = ptr(now.Add(2 * time.Hour))
			}
			if s.receipt != "" {
				r.ReceiptVerdict = s.receipt
			}
			if s.credit != "" {
				r.CreditState, r.CreditedMinutes = s.credit, s.minutes
				if s.credit == CreditFinal {
					r.CreditFinalAt = ptr(now.Add(2 * time.Hour))
				}
			}
		}))
	}
	return out
}

func TestCalibrationPerVerdictClass(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	seedReviews(t, f, st, []seed{
		{verdict: VerdictSafe, dba: DBAConcur, receipt: ReceiptClean},
		{verdict: VerdictSafe, dba: DBAConcur, receipt: ReceiptRegressed},
		{verdict: VerdictSafe, dba: DBAOverride, receipt: ReceiptPending},
		{verdict: VerdictSafe},
		{verdict: VerdictRisky, dba: DBAConcur, receipt: ReceiptClean},
		{verdict: VerdictRisky, receipt: ReceiptClean, age: 100 * 24 * time.Hour},
	})
	c, err := st.Calibration(f.ctx, time.Now().Add(-90*24*time.Hour))
	if err != nil {
		t.Fatalf("Calibration: %v", err)
	}
	safe := c.ByVerdict[VerdictSafe]
	if c.Reviews != 5 || safe.Reviews != 4 || safe.Decided != 3 || safe.Concurred != 2 ||
		safe.Receipts != 2 || safe.Clean != 1 || safe.Regressed != 1 || c.FalseSafe != 1 {
		t.Fatalf("calibration = %+v safe %+v", c, safe)
	}
	if r := c.ByVerdict[VerdictRisky]; r.Reviews != 1 || r.Concurred != 1 {
		t.Fatalf("risky = %+v (the 100-day-old review must be outside the window)", r)
	}
	line := c.Line()
	for _, want := range []string{"safe: 2 of 3 concurred", "1 false-safe", "risky: 1 of 1"} {
		if !strings.Contains(line, want) {
			t.Errorf("calibration line %q lacks %q", line, want)
		}
	}
}

func TestCalibrationEmpty(t *testing.T) {
	f := newFixture(t)
	c, err := NewStore(f.pool, "testdb", nil).Calibration(f.ctx, time.Now().Add(-time.Hour))
	if err != nil || c.Reviews != 0 || c.FalseSafe != 0 {
		t.Fatalf("empty calibration = %+v %v", c, err)
	}
	if !strings.Contains(c.Line(), "no decided reviews") {
		t.Fatalf("empty line = %q", c.Line())
	}
}

func TestSummaryMetrics(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	pr := "https://github.com/acme/shop/pull/9"
	cr03 := []Finding{ruleFinding("CR-03", SeverityCritical)}
	seedReviews(t, f, st, []seed{
		{verdict: VerdictSafe, dba: DBAConcur, receipt: ReceiptClean, credit: CreditFinal,
			minutes: 15, tokens: 1000, shadow: ptr(true), score: ShadowCorrect},
		{verdict: VerdictSafe, dba: DBAConcur, receipt: ReceiptRegressed, credit: CreditVoid,
			tokens: 3000, shadow: ptr(true), score: ShadowIncorrect},
		{verdict: VerdictRisky, dba: DBAConcur, credit: CreditPending, minutes: 15,
			pr: pr, findings: cr03, age: time.Hour},
		{verdict: VerdictReview, pr: pr},
		{verdict: VerdictRisky, dba: DBAOverride, findings: cr03},
	})
	sum, err := st.Summary(f.ctx, time.Now().Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Reviews != 5 || sum.VerdictMix[VerdictSafe] != 2 || sum.VerdictMix[VerdictRisky] != 2 {
		t.Fatalf("mix = %+v", sum.VerdictMix)
	}
	if sum.HoursCredited != 0.25 || sum.HoursPending != 0.25 || sum.FalseSafe != 1 {
		t.Fatalf("hours %v/%v false-safe %d", sum.HoursCredited, sum.HoursPending, sum.FalseSafe)
	}
	if sum.Receipts[ReceiptClean] != 1 || sum.Receipts[ReceiptRegressed] != 1 {
		t.Fatalf("receipts = %+v", sum.Receipts)
	}
	if sum.CaughtBeforeMerge != 1 {
		t.Fatalf("caught before merge = %d, want the risky review whose next revision "+
			"is review", sum.CaughtBeforeMerge)
	}
	o := sum.OverrideByRule["CR-03"]
	if o.Reviews != 2 || o.Overrides != 1 || o.Rate == nil || *o.Rate != 0.5 ||
		len(o.Reasons) != 1 || o.Reasons[0] != "reason risky" {
		t.Fatalf("CR-03 overrides = %+v", o)
	}
	if sum.TokensPerReview != 800 {
		t.Fatalf("tokens per review = %v, want 800", sum.TokensPerReview)
	}
	if sum.DBAWaitP50Seconds == nil || *sum.DBAWaitP50Seconds != 3540 {
		t.Fatalf("median time to DBA verdict = %v, want 59 minutes", sum.DBAWaitP50Seconds)
	}
	ac := sum.AutoClear
	if ac.Scored != 2 || ac.Correct != 1 || ac.Ready || ac.MinScored != 30 ||
		ac.MinCorrectRate != 0.97 || ac.MaxRegressions != 0 || ac.Regressions90d != 1 ||
		ac.Label != "shadow only — not enabled" {
		t.Fatalf("auto-clear readiness = %+v", ac)
	}
}

func TestSummaryRejectsFutureSince(t *testing.T) {
	f := newFixture(t)
	if _, err := NewStore(f.pool, "testdb", nil).Summary(f.ctx,
		time.Now().Add(time.Hour)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("future since = %v", err)
	}
}

func TestMergeSummaries(t *testing.T) {
	a := Summary{Databases: []string{"db1"}, Reviews: 2, VerdictMix: map[string]int{
		VerdictSafe: 2}, HoursCredited: 0.25, TokensPerReview: 100, FalseSafe: 1,
		OverrideByRule: map[string]RuleOverrides{"CR-03": {Reviews: 2, Overrides: 1,
			Reasons: []string{"a"}}}, dbaWaits: []float64{10, 30}}
	b := Summary{Databases: []string{"db2"}, Reviews: 1, VerdictMix: map[string]int{
		VerdictRisky: 1}, HoursCredited: 0.5, TokensPerReview: 400,
		OverrideByRule: map[string]RuleOverrides{"CR-03": {Reviews: 2, Overrides: 2,
			Reasons: []string{"b"}}}, dbaWaits: []float64{20}}
	m := MergeSummaries([]Summary{a, b}, []string{"db3"})
	if m.Reviews != 3 || m.VerdictMix[VerdictSafe] != 2 || m.VerdictMix[VerdictRisky] != 1 ||
		m.HoursCredited != 0.75 || m.FalseSafe != 1 || m.TokensPerReview != 200 {
		t.Fatalf("merged = %+v", m)
	}
	o := m.OverrideByRule["CR-03"]
	if o.Reviews != 4 || o.Overrides != 3 || *o.Rate != 0.75 || len(o.Reasons) != 2 {
		t.Fatalf("merged overrides = %+v", o)
	}
	if m.DBAWaitP50Seconds == nil || *m.DBAWaitP50Seconds != 20 {
		t.Fatalf("merged median = %v, want 20 over all samples", m.DBAWaitP50Seconds)
	}
	if !m.Partial || len(m.Unavailable) != 1 || len(m.Databases) != 2 {
		t.Fatalf("partial %v unavailable %v databases %v", m.Partial, m.Unavailable,
			m.Databases)
	}
	if e := MergeSummaries(nil, nil); e.Reviews != 0 || e.Partial {
		t.Fatalf("empty merge = %+v", e)
	}
}
