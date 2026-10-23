package changereview

import (
	"context"
	"fmt"
	"time"
)

// Submit validates and queues a review, or returns the completed (or
// still running) review of the same input_hash created within
// DedupeWindow with created=false. With WaitSeconds it waits that long
// for the review to complete; otherwise it returns the running review.
// Refusals: ErrDisabled, ErrInvalid (naming the limit or field), ErrBusy
// when the runner and its queue are full, ErrUnavailable before Start or
// when the store fails.
func (s *Service) Submit(ctx context.Context, c Caller, req Request) (Review, bool, error) {
	if !s.d.Config.Enabled {
		return Review{}, false, ErrDisabled
	}
	runCtx := s.runContext()
	if runCtx == nil {
		return Review{}, false, fmt.Errorf("%w: change review for %s has not started",
			ErrUnavailable, s.d.Database)
	}
	req, err := validateRequest(s.d.Config, s.d.Database, c, req)
	if err != nil {
		return Review{}, false, err
	}
	wait := time.Duration(req.WaitSeconds) * time.Second
	hash := InputHash(s.d.Database, req)
	prev, ok, err := s.store.findReusable(ctx, hash, s.d.Now().Add(-DedupeWindow))
	if err != nil {
		return Review{}, false, err
	}
	if ok {
		r, err := s.Wait(ctx, prev.ID, wait)
		return r, false, err
	}
	r, err := s.enqueue(ctx, runCtx, c, req, hash)
	if err != nil {
		return Review{}, false, err
	}
	if wait > 0 {
		r, err = s.Wait(ctx, r.ID, wait)
	}
	return r, true, err
}

func (s *Service) runContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctx
}

// enqueue admits, stores and starts one review.
func (s *Service) enqueue(ctx, runCtx context.Context, c Caller, req Request,
	hash string) (Review, error) {
	r := s.newReview(c, req, hash)
	if req.PR.URL != "" {
		prev, ok, err := s.store.latestForPR(ctx, req.PR.URL, zeroUUID)
		if err != nil {
			return Review{}, err
		}
		if ok {
			r.RevisionOf = prev.ID
		}
	}
	if !s.admit() {
		return Review{}, fmt.Errorf("%w: %d reviews of %s are running or queued; retry later",
			ErrBusy, s.d.Config.MaxConcurrentPerDatabase+QueueLength, s.d.Database)
	}
	stored, err := s.store.Insert(ctx, r)
	if err != nil {
		s.release("")
		return Review{}, err
	}
	done := make(chan struct{})
	s.mu.Lock()
	s.done[stored.ID] = done
	s.mu.Unlock()
	in := newInput(stored.ID, s.d.Database, c, req, s.d.Config)
	s.d.Go(func() { s.run(runCtx, stored, in) })
	return stored, nil
}

// admit takes a runner or queue slot.
func (s *Service) admit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending >= s.d.Config.MaxConcurrentPerDatabase+QueueLength {
		return false
	}
	s.pending++
	return true
}

// release frees a slot and wakes the waiters of id.
func (s *Service) release(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending--
	if done, ok := s.done[id]; ok {
		close(done)
		delete(s.done, id)
	}
}

// newReview is the running row of a validated request; the raw diff is
// kept only as its sha256.
func (s *Service) newReview(c Caller, req Request, hash string) Review {
	now := utc(s.d.Now())
	return Review{Database: s.d.Database, Source: c.Source, Actor: c.Actor,
		Repo: req.PR.Repo, PRURL: req.PR.URL, PRNumber: req.PR.Number,
		HeadSHA: req.PR.HeadSHA, BaseSHA: req.PR.BaseSHA,
		PRTitle: s.redact(req.PR.Title), PROpenedAt: utcPtr(req.PR.OpenedAt),
		MigrationTool: req.MigrationTool, Transactional: req.Transactional,
		InputHash: hash, DiffSHA256: DiffSHA256(req.Diff), State: StateRunning,
		Stage: StageQueued, ShadowScore: ShadowUnscored, CreditState: CreditNone,
		CreatedAt: now, UpdatedAt: now}
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := utc(*t)
	return &u
}

// ReportDeployed records that a reviewed change was deployed: the review
// named by ID, or the newest complete review of PRURL, lands now with
// source reported. A review that already landed is ErrAlreadyDecided.
func (s *Service) ReportDeployed(ctx context.Context, c Caller, d Deployment) (Review,
	error) {
	if !s.d.Config.Enabled {
		return Review{}, ErrDisabled
	}
	if err := s.checkDeployment(c, d); err != nil {
		return Review{}, err
	}
	r, err := s.deployedReview(ctx, d)
	if err != nil {
		return Review{}, err
	}
	if r.State != StateComplete {
		return Review{}, invalid("review %s is %s; only a complete review lands", r.ID,
			r.State)
	}
	if d.DeployedAt.Before(r.CreatedAt.Add(-DedupeWindow)) {
		return Review{}, invalid("deployed_at %s is long before the review",
			d.DeployedAt.UTC().Format(time.RFC3339))
	}
	at := utc(d.DeployedAt)
	return s.store.MarkLanded(ctx, r.ID, Landing{Source: LandingSourceReported, Lower: at,
		At: at})
}

func (s *Service) checkDeployment(c Caller, d Deployment) error {
	if err := checkCaller(c); err != nil {
		return err
	}
	switch {
	case d.DeployedAt.IsZero():
		return invalid("deployed_at is required")
	case d.DeployedAt.After(s.d.Now().Add(clockSkew)):
		return invalid("deployed_at %s is in the future",
			d.DeployedAt.UTC().Format(time.RFC3339))
	case d.Commit != "" && !shaPattern.MatchString(d.Commit):
		return invalid("commit must be 7-64 lower-case hex characters")
	case d.ID == "" && d.PRURL == "":
		return invalid("a deploy report names a review id or a pr_url")
	case d.ID == "" && checkPRURL(d.PRURL) != nil:
		return checkPRURL(d.PRURL)
	}
	return nil
}

func (s *Service) deployedReview(ctx context.Context, d Deployment) (Review, error) {
	if d.ID != "" {
		return s.store.Get(ctx, d.ID)
	}
	r, ok, err := s.store.latestCompleteForPR(ctx, d.PRURL)
	if err != nil {
		return Review{}, err
	}
	if !ok {
		return Review{}, fmt.Errorf("%w: no complete review of %s", ErrNotFound, d.PRURL)
	}
	return r, nil
}
