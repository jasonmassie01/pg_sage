package shadow

import (
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// Options tune the shadow ledger. Zero values take the defaults.
type Options struct {
	// DedupeWindow: at most one decision per fingerprint per window.
	DedupeWindow time.Duration
	// ScoreAfter is how long a decision waits for the operator or an
	// applied change before the what-if may score it.
	ScoreAfter time.Duration
	// Horizon: a decision no evidence matched by then is unscored.
	Horizon time.Duration
	// MaxWait: a decision whose matched evidence never decided (a
	// verification that never finished) is unscored by then.
	MaxWait time.Duration
	// VerifyWindow and VerifyMaxWindow bound the after-window of a change
	// applied outside pg_sage (DropWindow for index drops, their business
	// cycle); Thresholds are the verification bars.
	VerifyWindow, VerifyMaxWindow, DropWindow time.Duration
	Thresholds                                verify.Thresholds
	// HypoPGMinPct is the what-if improvement an index create needs.
	HypoPGMinPct float64
	// HypoPGBudget bounds the what-if evaluations of one pass; Batch the
	// pending decisions one pass reads.
	HypoPGBudget, Batch int
	// Database names the database in metrics.
	Database string
}

// DefaultOptions: one decision per fingerprint a day, the what-if after a
// day, unscored after a week (three when evidence is still deciding),
// verification windows as pg_sage's own (1 h to 72 h; drops 7 days).
func DefaultOptions() Options {
	return Options{DedupeWindow: 24 * time.Hour, ScoreAfter: 24 * time.Hour,
		Horizon: 7 * 24 * time.Hour, MaxWait: 21 * 24 * time.Hour,
		VerifyWindow: time.Hour, VerifyMaxWindow: 72 * time.Hour,
		DropWindow: 7 * 24 * time.Hour, HypoPGMinPct: 10, HypoPGBudget: 10, Batch: 200}
}

// normalized replaces zero or inconsistent values with the defaults.
func (o Options) normalized() Options {
	def := DefaultOptions()
	for _, d := range []struct{ got, def *time.Duration }{
		{&o.DedupeWindow, &def.DedupeWindow}, {&o.ScoreAfter, &def.ScoreAfter},
		{&o.Horizon, &def.Horizon}, {&o.MaxWait, &def.MaxWait},
		{&o.VerifyWindow, &def.VerifyWindow}, {&o.VerifyMaxWindow, &def.VerifyMaxWindow},
		{&o.DropWindow, &def.DropWindow},
	} {
		if *d.got <= 0 {
			*d.got = *d.def
		}
	}
	o.Horizon = max(o.Horizon, o.ScoreAfter)
	o.MaxWait = max(o.MaxWait, o.Horizon)
	o.VerifyMaxWindow = max(o.VerifyMaxWindow, o.VerifyWindow)
	if !(o.HypoPGMinPct > 0) {
		o.HypoPGMinPct = def.HypoPGMinPct
	}
	if o.HypoPGBudget <= 0 {
		o.HypoPGBudget = def.HypoPGBudget
	}
	if o.Batch <= 0 {
		o.Batch = def.Batch
	}
	return o
}

// What-if verdicts (optimizer.WhatIfVerified, ...).
const (
	hypoVerified   = "verified"
	hypoRejected   = "rejected"
	hypoUnverified = "unverified"
)

// queueFact is the operator's decision on the same proposal.
type queueFact struct {
	ID          int64
	Status      string
	ActionLogID int64
	Verdict     string
	Lifecycle   string
}

// actionFact is the same change, run later through pg_sage.
type actionFact struct {
	ID        int64
	Verdict   string
	Lifecycle string
}

// verifiedFact is the verification of a change applied outside pg_sage;
// Final once the verdict can no longer change.
type verifiedFact struct {
	Verdict  string
	Final    bool
	Reason   string
	Evidence map[string]any
}

// hypoFact is a what-if evaluation of an index create.
type hypoFact struct {
	Verdict     string
	Improvement float64
	Reason      string
}

// facts is every piece of evidence found for one decision.
type facts struct {
	queue    *queueFact
	action   *actionFact
	external *verifiedFact
	hypo     *hypoFact
}

// judgement is a scoring outcome, or Wait for more evidence.
type judgement struct {
	Wait                       bool
	Score, Source, Reason      string
	Counted                    bool
	RefActionLogID, RefQueueID int64
	Detail                     map[string]any
}

// judge scores d from ev by precedence: the operator's decision, the
// same change applied (through pg_sage, then outside it), the what-if
// (index creates, after ScoreAfter), then unscored at the horizon.
// Matched evidence that is still deciding is waited for, up to MaxWait.
func judge(d Decision, ev facts, age time.Duration, o Options) judgement {
	if j, matched := judgeOperator(d, ev.queue); matched {
		return waitCapped(j, age, o)
	}
	if a := ev.action; a != nil {
		j := fromVerdict(d, SourceApplied, a.Verdict, a.Lifecycle)
		j.RefActionLogID = a.ID
		return waitCapped(j, age, o)
	}
	if x := ev.external; x != nil {
		return waitCapped(fromExternal(d, x), age, o)
	}
	if h := ev.hypo; h != nil && d.Class == "index_create" && age >= o.ScoreAfter {
		switch h.Verdict {
		case hypoVerified:
			return hypoJudgement(ScoreCorrect, h)
		case hypoRejected:
			return hypoJudgement(ScoreIncorrect, h)
		}
	}
	if age >= o.Horizon {
		return judgement{Score: ScoreUnscored, Source: SourceNone,
			Reason: "no operator decision, applied change or what-if before the horizon"}
	}
	return judgement{Wait: true}
}

// judgeOperator judges a decided proposal; matched is false for one the
// operator did not decide (pending, expired, superseded, failed).
func judgeOperator(d Decision, q *queueFact) (judgement, bool) {
	if q == nil {
		return judgement{}, false
	}
	switch q.Status {
	case "rejected":
		return judgement{Score: ScoreIncorrect, Source: SourceOperator, RefQueueID: q.ID,
			Reason: fmt.Sprintf("an operator rejected the proposal (queue item %d)",
				q.ID)}, true
	case "approved", "executed":
		if q.ActionLogID <= 0 {
			return judgement{Wait: true}, true
		}
		j := fromVerdict(d, SourceOperator, q.Verdict, q.Lifecycle)
		j.RefQueueID, j.RefActionLogID = q.ID, q.ActionLogID
		return j, true
	}
	return judgement{}, false
}

// waitCapped turns a wait on matched evidence into unscored at MaxWait.
func waitCapped(j judgement, age time.Duration, o Options) judgement {
	if j.Wait && age >= o.MaxWait {
		return judgement{Score: ScoreUnscored, Source: SourceNone,
			Reason: "the matched evidence never reached a verdict"}
	}
	return j
}

func fromVerdict(d Decision, source, verdict, lifecycle string) judgement {
	score, decided := scoreForVerdict(d.Family, d.Class, verdict, lifecycle)
	if !decided {
		return judgement{Wait: true}
	}
	return judgement{Score: score, Source: source, Counted: false,
		Reason: fmt.Sprintf("%s: verdict %q, lifecycle %q", source, verdict, lifecycle)}
}

func fromExternal(d Decision, x *verifiedFact) judgement {
	if !x.Final {
		return judgement{Wait: true}
	}
	score, decided := scoreForVerdict(d.Family, d.Class, x.Verdict, "success")
	if !decided {
		score = ScoreUnscored
	}
	return judgement{Score: score, Source: SourceExternal, Counted: score != ScoreUnscored,
		Reason: "applied outside pg_sage; verification: " + x.Reason, Detail: x.Evidence}
}

func hypoJudgement(score string, h *hypoFact) judgement {
	return judgement{Score: score, Source: SourceHypoPG, Counted: true,
		Reason: fmt.Sprintf("what-if %s: %.1f%% cheaper %s", h.Verdict, h.Improvement,
			h.Reason),
		Detail: map[string]any{"verdict": h.Verdict, "improvement_pct": h.Improvement}}
}

// scoreForVerdict is the score of a decision whose change ran with a
// verdict (sage.action_outcome) and a lifecycle (action_log.outcome), by
// the family's rule; decided is false while the verdict may still come.
// An operator's rollback is an incorrect decision, except the rollback of
// its own regression (already incorrect) and the no-gain revert of a
// neutral index create (neutral).
func scoreForVerdict(family, class, verdict, lifecycle string) (string, bool) {
	if lifecycle == "rolled_back" && verdict != verify.OutcomeRegressed &&
		!(verdict == verify.OutcomeNeutral && class == "index_create") {
		return ScoreIncorrect, true
	}
	switch verdict {
	case verify.OutcomeImproved:
		return ScoreCorrect, true
	case verify.OutcomeRegressed:
		return ScoreIncorrect, true
	case verify.OutcomeNeutral:
		if family == "hygiene" && lifecycle != "rolled_back" && lifecycle != "rollback_failed" {
			return ScoreCorrect, true
		}
		return ScoreNeutral, true
	case verify.OutcomeInsufficient, verify.OutcomeUnverifiable:
		return ScoreUnscored, true
	}
	return "", false
}
