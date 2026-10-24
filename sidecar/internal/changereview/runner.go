package changereview

import (
	"context"
	"errors"
	"fmt"
)

// run is one review on the runner: wait for a slot, run the deterministic
// stage (stored first), then the model stage, merge, render and complete.
// Every failure ends the review failed with its reason; nothing is left
// running.
func (s *Service) run(ctx context.Context, r Review, in Input) {
	defer s.release(r.ID)
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		s.failReview(r.ID, FailureInterrupted)
		return
	}
	defer func() { <-s.sem }()
	if err := s.process(ctx, r, in); err != nil {
		s.d.Logf("change review: %s: review %s failed: %v", s.d.Database, r.ID, err)
		s.failReview(r.ID, err.Error())
	}
}

// failReview stores a failure even after the run's context ended.
func (s *Service) failReview(id, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	if _, err := s.store.fail(ctx, id, reason); err != nil &&
		!errors.Is(err, ErrAlreadyDecided) {
		s.d.Logf("change review: %s: could not mark review %s failed (%s): %v",
			s.d.Database, id, reason, err)
	}
}

func (s *Service) process(ctx context.Context, r Review, in Input) error {
	if err := s.store.setStage(ctx, r.ID, StageDeterministic); err != nil {
		return err
	}
	a, err := s.analyze(ctx, in)
	if err != nil {
		return err
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	det := Merge(a, nil)
	if err := s.redactMerged(&det); err != nil {
		return err
	}
	if err := s.store.saveDeterministic(persist, r.ID, det, a.ParseMode); err != nil {
		return err
	}
	m := Merge(a, s.reason(ctx, in, a))
	if err := s.redactMerged(&m); err != nil {
		return err
	}
	done, err := s.finalize(persist, r, in, a, m)
	if err != nil {
		return err
	}
	stored, err := s.store.complete(persist, done)
	if err != nil {
		return err
	}
	if s.d.Notifier != nil && stored.Verdict != VerdictSafe && stored.DBAVerdict == "" &&
		!s.alreadyPaged(persist, stored) {
		s.d.Notifier.ReviewNeedsDBA(persist, stored)
	}
	return nil
}

// alreadyPaged reports whether the DBA was already asked about this PR:
// an earlier revision at the same or a worse verdict still waits for a
// verdict. A read failure pages (better twice than never).
func (s *Service) alreadyPaged(ctx context.Context, r Review) bool {
	if r.PRURL == "" {
		return false
	}
	paged, err := s.store.waitingAtLeast(ctx, r)
	if err != nil {
		s.d.Logf("change review: %s: earlier revisions of %s unreadable, paging: %v",
			s.d.Database, r.ID, err)
		return false
	}
	return paged
}

// analyze runs the deterministic stage under its time limit.
func (s *Service) analyze(ctx context.Context, in Input) (Analysis, error) {
	actx, cancel := context.WithTimeout(ctx, s.d.DeterministicTimeout)
	defer cancel()
	a, err := s.d.Analyzer.Analyze(actx, in)
	switch {
	case err != nil && errors.Is(actx.Err(), context.DeadlineExceeded) &&
		ctx.Err() == nil:
		return Analysis{}, fmt.Errorf("deterministic stage timed out after %v",
			s.d.DeterministicTimeout)
	case err != nil && ctx.Err() != nil:
		return Analysis{}, errors.New(FailureInterrupted)
	case err != nil:
		return Analysis{}, fmt.Errorf("deterministic stage failed: %w", err)
	}
	if err := validateAnalysis(a); err != nil {
		return Analysis{}, fmt.Errorf("deterministic stage returned an invalid analysis: %w",
			err)
	}
	return a, nil
}

// reason runs the model stage under its time limit; provider and stage
// trouble become a model status, never a failed review.
func (s *Service) reason(ctx context.Context, in Input, a Analysis) *Reasoning {
	if s.d.Reasoner == nil {
		return nil
	}
	mctx, cancel := context.WithTimeout(ctx, s.d.ModelTimeout)
	defer cancel()
	res, err := s.d.Reasoner.Reason(mctx, in, a)
	if err == nil {
		return &res
	}
	status := ModelError
	if errors.Is(mctx.Err(), context.DeadlineExceeded) {
		status = ModelTimeout
	}
	s.d.Logf("change review: %s: model stage of review %s: %s: %v", s.d.Database,
		in.ReviewID, status, err)
	return &Reasoning{ModelStatus: status}
}

// validateAnalysis refuses an analysis the merge cannot trust.
func validateAnalysis(a Analysis) error {
	if a.ParseMode != ParseAST && a.ParseMode != ParseText {
		return fmt.Errorf("parse mode %q is not ast or text", a.ParseMode)
	}
	for _, f := range a.Findings {
		switch {
		case f.Origin != OriginRule || f.RuleID == "":
			return fmt.Errorf("finding %q is not a rule finding", f.Title)
		case f.Severity != SeverityInfo && f.Severity != SeverityWarning &&
			f.Severity != SeverityCritical:
			return fmt.Errorf("finding %s has severity %q", f.RuleID, f.Severity)
		}
	}
	for _, e := range a.Evidence {
		if !ValidEvidenceID(e.ID) {
			return fmt.Errorf("evidence id %q", e.ID)
		}
	}
	if n := len(a.Expectations.WatchQueryIDs); n > MaxWatchQueryIDs {
		return fmt.Errorf("watch set of %d queryids, more than %d", n, MaxWatchQueryIDs)
	}
	return nil
}

// finalize is the completed review: merged results (redacted), summary,
// content hash, a carried-over DBA verdict, the shadow flag and the
// rendered comment.
func (s *Service) finalize(ctx context.Context, r Review, in Input, a Analysis,
	m Merged) (Review, error) {
	r.Verdict, r.DeterministicVerdict, r.ParseMode = m.Verdict, m.DeterministicVerdict,
		a.ParseMode
	r.ModelStatus, r.Tokens, r.Transcript = m.ModelStatus, m.Tokens, m.Transcript
	r.Statements, r.Findings, r.Evidence = m.Statements, m.Findings, m.Evidence
	r.Dropped, r.NotDetermined, r.Expectations = m.Dropped, m.NotDetermined, a.Expectations
	if r.ModelStatus != ModelOK && in.Diff != "" {
		r.NotDetermined = append(r.NotDetermined, NotDetermined{Subject: "app-code changes",
			Reason: "not analyzed (model " + r.ModelStatus + ")"})
	}
	r.Summary = s.summarize(ctx, r, in, m)
	r.ContentHash = ContentHash(in.SQLFiles, s.dbHunks(in.Diff))
	if err := s.carryVerdict(ctx, &r); err != nil {
		return r, err
	}
	if s.d.AutoClear != nil {
		v := s.d.AutoClear(r)
		r.ShadowAutoClear = &v
	}
	now := utc(s.d.Now())
	r.State, r.Stage, r.CompletedAt, r.CreditState = StateComplete, StageDone, &now,
		CreditNone
	r.LandingState, r.ReceiptVerdict = LandingAwaiting, ReceiptPending
	if a.Expectations.AtReview == AtReviewLanded {
		// The change was already there when it was reviewed: it can never
		// land, so it earns no receipt and no credit.
		r.LandingState = LandingNotApplicable
	}
	r.Markdown = RenderMarkdown(r, s.d.Render)
	return r, nil
}

func (s *Service) dbHunks(diff string) string {
	if s.d.HunkFilter == nil {
		return diff
	}
	return s.d.HunkFilter(diff)
}

// carryVerdict copies the DBA verdict of an earlier revision of the same
// PR with an equal content hash. The credit stays with that review.
func (s *Service) carryVerdict(ctx context.Context, r *Review) error {
	if r.PRURL == "" {
		return nil
	}
	src, ok, err := s.store.carrySource(ctx, r.PRURL, r.ContentHash, r.ID)
	if err != nil || !ok {
		return err
	}
	r.DBAVerdict, r.DBADirection, r.DBAReason = src.DBAVerdict, src.DBADirection,
		src.DBAReason
	r.DBAActor, r.DBADecidedAt, r.VerdictCarriedFrom = src.DBAActor, src.DBADecidedAt,
		src.ID
	return nil
}

// summarize is the per-review digest, with the database's track record.
func (s *Service) summarize(ctx context.Context, r Review, in Input, m Merged) ReviewSummary {
	sum := ReviewSummary{Rules: []string{}, DroppedCount: len(r.Dropped), Intent: m.Intent,
		AppCodeAnalyzed: r.ModelStatus == ModelOK && in.Diff != ""}
	seen := map[string]bool{}
	for _, f := range r.Findings {
		switch f.Severity {
		case SeverityCritical:
			sum.Critical++
		case SeverityWarning:
			sum.Warning++
		default:
			sum.Info++
		}
		if f.Origin == OriginModel {
			sum.ModelFindings++
		}
		if f.RuleID != "" && !seen[f.RuleID] {
			seen[f.RuleID] = true
			sum.Rules = append(sum.Rules, f.RuleID)
		}
	}
	cal, err := s.store.Calibration(ctx, s.d.Now().Add(-CalibrationWindow))
	if err != nil {
		s.d.Logf("change review: %s: calibration unavailable for review %s: %v",
			s.d.Database, r.ID, err)
		return sum
	}
	sum.Calibration = &cal
	return sum
}
