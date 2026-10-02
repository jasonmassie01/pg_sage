package earned

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// nonHumanActors can never approve a promotion: the approval is the
// trust step, so pg_sage and agents acting through MCP cannot take it.
var nonHumanPrefixes = []string{ActorPgSage, "system", "mcp:", "mcp-agent", "stdio"}

func humanActor(actor string) bool {
	actor = strings.TrimSpace(actor)
	if actor == "" || len(actor) > 200 {
		return false
	}
	for _, prefix := range nonHumanPrefixes {
		if strings.HasPrefix(actor, prefix) {
			return false
		}
	}
	return true
}

// Approve applies a pending promotion as approver. The evidence is held
// against the target again: a proposal whose evidence decayed cannot
// take effect. The approval and the level change commit together.
func (s *Service) Approve(ctx context.Context, id, approver, note string) (State, error) {
	if !humanActor(approver) {
		return State{}, fmt.Errorf("%w: approver %q", ErrHumanApprovalRequired, approver)
	}
	if len(note) > 2000 {
		return State{}, fmt.Errorf("%w: note longer than 2000 characters", ErrInvalidRequest)
	}
	p, err := s.store.Proposal(ctx, id)
	if err != nil {
		return State{}, err
	}
	if err := s.checkDecidable(ctx, p); err != nil {
		return State{}, err
	}
	ev, err := s.Evidence(ctx, p.Family, p.Class)
	if err != nil {
		return State{}, err
	}
	a := Assess(s.cfg.Thresholds, p.To, ev)
	if !a.Met {
		return State{}, &EvidenceNotMetError{Assessment: a}
	}
	return s.applyApproval(ctx, p, approver, note, ev, a)
}

// checkDecidable refuses a decided proposal and expires a stale one.
func (s *Service) checkDecidable(ctx context.Context, p Proposal) error {
	if p.Status != StatusPending {
		return fmt.Errorf("%w: %s is %s", ErrNotPending, p.ID, p.Status)
	}
	if !s.now().Before(p.ExpiresAt) {
		if err := s.expire(ctx); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s expired at %s", ErrProposalExpired, p.ID, p.ExpiresAt)
	}
	if p.To > CapFor(p.Class) || !p.To.Grantable() || !Applicable(p.Family, p.Class) {
		return fmt.Errorf("%w: %s above the cap of %s", ErrInvalidRequest, p.To, p.Class)
	}
	return nil
}

func (s *Service) applyApproval(ctx context.Context, p Proposal, approver, note string,
	ev Evidence, a Assessment) (State, error) {
	raw, err := json.Marshal(struct {
		Evidence   Evidence   `json:"evidence"`
		Assessment Assessment `json:"assessment"`
	}{ev, a})
	if err != nil {
		return State{}, fmt.Errorf("%w: encode evidence: %v", ErrInvalidRequest, err)
	}
	now := s.now()
	var out State
	err = s.store.withTx(ctx, func(tx pgx.Tx) error {
		locked, err := s.store.readProposal(ctx, tx, p.ID, true)
		if err != nil {
			return err
		}
		if locked.Status != StatusPending {
			return fmt.Errorf("%w: %s is %s", ErrNotPending, p.ID, locked.Status)
		}
		current, found, err := s.store.readLevel(ctx, tx, p.Family, p.Class)
		if err != nil {
			return err
		}
		if (found && current.Level != p.From) || (!found && defaultLevel(p.Family) != p.From) {
			return fmt.Errorf("%w: the level moved since the proposal", ErrConflict)
		}
		out, err = s.store.writeLevel(ctx, tx, levelChange{Family: p.Family, Class: p.Class,
			To: p.To, ExpectVersion: current.Version, Actor: approver,
			Reason: "promotion " + p.ID + " approved", Evidence: raw, At: now})
		if err != nil {
			return err
		}
		if err := s.store.decideProposal(ctx, tx, p.ID, StatusApproved, approver, note,
			now); err != nil {
			return err
		}
		return s.store.appendEvent(ctx, tx, Event{Family: p.Family, Class: p.Class,
			Type: EventPromotionApproved, From: levelPtr(p.From), To: levelPtr(p.To),
			Actor: approver, Reason: noteOr(note, "approved"), ProposalID: p.ID, Evidence: raw,
			At: now})
	})
	s.invalidate()
	return out, err
}

// Reject declines a pending promotion; the level does not change.
func (s *Service) Reject(ctx context.Context, id, actor, note string) (Proposal, error) {
	if strings.TrimSpace(actor) == "" || len(actor) > 200 || len(note) > 2000 {
		return Proposal{}, fmt.Errorf("%w: a reject needs an actor and a short note",
			ErrInvalidRequest)
	}
	p, err := s.store.Proposal(ctx, id)
	if err != nil {
		return Proposal{}, err
	}
	now := s.now()
	err = s.store.withTx(ctx, func(tx pgx.Tx) error {
		if err := s.store.decideProposal(ctx, tx, id, StatusRejected, actor, note,
			now); err != nil {
			return err
		}
		return s.store.appendEvent(ctx, tx, Event{Family: p.Family, Class: p.Class,
			Type: EventPromotionRejected, From: levelPtr(p.From), To: levelPtr(p.To),
			Actor: actor, Reason: noteOr(note, "rejected"), ProposalID: id, At: now})
	})
	if err != nil {
		return Proposal{}, err
	}
	return s.store.Proposal(ctx, id)
}

func noteOr(note, fallback string) string {
	if strings.TrimSpace(note) == "" {
		return fallback
	}
	return note
}

// DowngradeRequest lowers a pair, or every class of a family
// (Class = AllClasses), to To.
type DowngradeRequest struct {
	Family Family      `json:"family"`
	Class  ActionClass `json:"class"`
	To     Level       `json:"to"`
	Actor  string      `json:"-"`
	Reason string      `json:"reason"`
}

func (r DowngradeRequest) validate() error {
	switch {
	case !KnownFamily(r.Family):
		return fmt.Errorf("%w: unknown family %q", ErrInvalidRequest, r.Family)
	case r.Class != AllClasses && !Applicable(r.Family, r.Class):
		return fmt.Errorf("%w: %q is not a class of %s", ErrInvalidRequest, r.Class, r.Family)
	case !r.To.Grantable():
		return fmt.Errorf("%w: level %d", ErrInvalidRequest, int(r.To))
	case strings.TrimSpace(r.Actor) == "" || len(r.Actor) > 200:
		return fmt.Errorf("%w: a downgrade needs an actor", ErrInvalidRequest)
	case strings.TrimSpace(r.Reason) == "" || len(r.Reason) > 1000:
		return fmt.Errorf("%w: a downgrade needs a reason", ErrInvalidRequest)
	}
	return nil
}

// Downgrade lowers levels at once (restricting is always allowed) and
// supersedes pending promotions of the lowered pairs.
func (s *Service) Downgrade(ctx context.Context, r DowngradeRequest) ([]State, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	classes := []ActionClass{r.Class}
	if r.Class == AllClasses {
		classes = ApplicableClasses(r.Family)
	}
	out, err := s.lower(ctx, r.Family, classes, r.To, r.Actor, r.Reason, EventDowngraded)
	if err == nil && len(out) == 0 {
		return nil, fmt.Errorf("%w: %s/%s is not above %s", ErrNotADowngrade, r.Family,
			r.Class, r.To)
	}
	return out, err
}

// lower moves every listed class above to down to it, in one transaction.
func (s *Service) lower(ctx context.Context, f Family, classes []ActionClass, to Level,
	actor, reason string, kind EventType) ([]State, error) {
	var out []State
	err := s.store.withTx(ctx, func(tx pgx.Tx) error {
		out = nil
		for _, c := range classes {
			st, changed, err := s.lowerOne(ctx, tx, f, c, to, actor, reason, kind)
			if err != nil {
				return err
			}
			if changed {
				out = append(out, st)
			}
		}
		return nil
	})
	s.invalidate()
	return out, err
}

func (s *Service) lowerOne(ctx context.Context, tx pgx.Tx, f Family, c ActionClass,
	to Level, actor, reason string, kind EventType) (State, bool, error) {
	now := s.now()
	current, found, err := s.store.readLevel(ctx, tx, f, c)
	if err != nil {
		return State{}, false, err
	}
	from := defaultLevel(f)
	if found {
		from = current.Level
	}
	// A safety regression caps a carried-over pair for the safety window
	// (a transient signal) instead of demoting it: it was granted by
	// policy and returns by itself. An operator's downgrade still applies.
	carried := found && current.Provenance == ProvenanceCarriedOver
	if from <= to || (carried && kind == EventAutoDowngraded) {
		return State{}, false, nil
	}
	st, err := s.store.writeLevel(ctx, tx, levelChange{Family: f, Class: c, To: to,
		ExpectVersion: current.Version, Actor: actor, Reason: reason, At: now})
	if err != nil {
		return State{}, false, err
	}
	if _, err := s.store.closePending(ctx, tx, f, c, StatusSuperseded, actor,
		"superseded by "+string(kind), now); err != nil {
		return State{}, false, err
	}
	err = s.store.appendEvent(ctx, tx, Event{Family: f, Class: c, Type: kind,
		From: levelPtr(from), To: levelPtr(to), Actor: actor, Reason: reason, At: now})
	return st, err == nil, err
}

// validateOutcome checks an outcome before it is recorded.
func validateOutcome(o Outcome) error {
	switch {
	case o.Result != ResultVerifiedRecovery && o.Result != ResultNotRecovered &&
		o.Result != ResultHarmful && o.Result != ResultSafetyViolation:
		return fmt.Errorf("%w: result %q", ErrInvalidRequest, o.Result)
	case o.Source != SourceExecutor && o.Source != SourceOperator &&
		o.Source != SourceGameDay && o.Source != SourceRollout && o.Source != SourceBench:
		return fmt.Errorf("%w: source %q", ErrInvalidRequest, o.Source)
	case !KnownFamily(o.Family) || !knownClass(o.Class):
		return fmt.Errorf("%w: pair %s/%s", ErrInvalidRequest, o.Family, o.Class)
	case !o.Level.Grantable():
		return fmt.Errorf("%w: level %d", ErrInvalidRequest, int(o.Level))
	case strings.TrimSpace(o.Database) == "" || len(o.Database) > 200:
		return fmt.Errorf("%w: database", ErrInvalidRequest)
	case strings.TrimSpace(o.Actor) == "" || len(o.Actor) > 200 || len(o.Detail) > 2000:
		return fmt.Errorf("%w: actor or detail", ErrInvalidRequest)
	}
	return nil
}

// RecordOutcome records a live outcome. A harmful or unsafe outcome
// demotes every class of the family to at most L1 at once; re-promotion
// needs a clean safety window again.
func (s *Service) RecordOutcome(ctx context.Context, o Outcome) error {
	_, err := s.recordOutcome(ctx, o)
	return err
}

// RecordOutcomeOnce records o and reports whether it was new: an outcome
// of an action already recorded from the same source is not recorded
// again (and does not demote twice).
func (s *Service) RecordOutcomeOnce(ctx context.Context, o Outcome) (bool, error) {
	return s.recordOutcome(ctx, o)
}

// recordOutcome records o and reports whether it was new.
func (s *Service) recordOutcome(ctx context.Context, o Outcome) (bool, error) {
	if err := validateOutcome(o); err != nil {
		return false, err
	}
	o.At = s.now()
	inserted, err := s.store.insertOutcome(ctx, o)
	s.invalidate()
	if err != nil || !inserted {
		return false, err
	}
	if o.Result != ResultHarmful && o.Result != ResultSafetyViolation {
		return true, nil
	}
	return true, s.demoteFamily(ctx, o)
}

// demoteFamily lowers every class of o.Family to at most L1.
func (s *Service) demoteFamily(ctx context.Context, o Outcome) error {
	reason := fmt.Sprintf("family safety regression: %s %s on %s (%s)", o.Class, o.Result,
		o.Database, o.Source)
	_, err := s.lower(ctx, o.Family, ApplicableClasses(o.Family), L1, ActorPgSage,
		truncate(reason, 1000), EventAutoDowngraded)
	if errors.Is(err, ErrConflict) {
		_, err = s.lower(ctx, o.Family, ApplicableClasses(o.Family), L1, ActorPgSage,
			truncate(reason, 1000), EventAutoDowngraded)
	}
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// RecordReview records an operator's verdict on an investigation packet.
func (s *Service) RecordReview(ctx context.Context, r Review) error {
	switch {
	case r.Verdict != VerdictAccepted && r.Verdict != VerdictRejected:
		return fmt.Errorf("%w: verdict %q", ErrInvalidRequest, r.Verdict)
	case !KnownFamily(r.Family):
		return fmt.Errorf("%w: family %q", ErrInvalidRequest, r.Family)
	case !uuidPattern.MatchString(r.InvestigationID):
		return fmt.Errorf("%w: investigation id", ErrInvalidRequest)
	case strings.TrimSpace(r.Reviewer) == "" || len(r.Reviewer) > 200:
		return fmt.Errorf("%w: reviewer", ErrInvalidRequest)
	case strings.TrimSpace(r.Database) == "" || len(r.Database) > 200 || len(r.Note) > 2000:
		return fmt.Errorf("%w: database or note", ErrInvalidRequest)
	}
	r.At = s.now()
	err := s.store.upsertReview(ctx, r)
	s.invalidate()
	return err
}

// IngestEvalRun stores a PGIncidentBench report as bench or game-day
// evidence. A game-day report names the database it ran against.
func (s *Service) IngestEvalRun(ctx context.Context, raw []byte, source, actor,
	database string) (EvalRun, error) {
	switch {
	case source != SourceBench && source != SourceGameDay:
		return EvalRun{}, fmt.Errorf("%w: source %q", ErrInvalidRequest, source)
	case (source == SourceGameDay) != (strings.TrimSpace(database) != ""):
		return EvalRun{}, fmt.Errorf("%w: a game day names its database, a bench does not",
			ErrInvalidRequest)
	case strings.TrimSpace(actor) == "" || len(actor) > 200 || len(database) > 200:
		return EvalRun{}, fmt.Errorf("%w: actor", ErrInvalidRequest)
	}
	run, err := ParseBenchReport(raw, s.now())
	if err != nil {
		return EvalRun{}, err
	}
	run.ID, run.Source, run.IngestedAt, run.IngestedBy, run.Database = newID(), source,
		s.now(), actor, database
	stored, err := s.store.insertEvalRun(ctx, run)
	s.invalidate()
	return stored, err
}
