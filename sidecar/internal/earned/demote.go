package earned

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Demotion on a demerit (roadmap 1.2): a regressed verdict, an operator's
// rollback or rejection of a class demotes that pair one level, at once,
// recorded with its cause, and an operator is told. Incident families
// keep their stronger rule for harmful outcomes (every class to L1); an
// operator's rejection of their handoff demotes the pair one level too.

// Demotion is one automatic demotion.
type Demotion struct {
	Database    string      `json:"database"`
	Family      Family      `json:"family"`
	Class       ActionClass `json:"class"`
	From        Level       `json:"from"`
	To          Level       `json:"to"`
	Cause       string      `json:"cause"`
	ActionLogID int64       `json:"action_log_id,omitempty"`
	QueueID     int64       `json:"queue_id,omitempty"`
	Detail      string      `json:"detail"`
	At          time.Time   `json:"at"`
}

// DemotionNotifier tells an operator about a demotion (the reconciler's
// notifier implements it when it can).
type DemotionNotifier interface {
	NotifyDemotion(ctx context.Context, d Demotion) error
}

// DemotionTarget is one level below from; L1 (a manual script, the
// default) and L0 are the floor.
func DemotionTarget(from Level) Level {
	if from <= L1 {
		return from
	}
	return from - 1
}

// demeritCause names the demerit an outcome is, if any: for a
// self-initiated pair a harmful (regressed) or rejected (operator
// rollback or rejection) outcome; for an incident pair only a rejection
// (its harmful outcomes demote the whole family).
func demeritCause(o Outcome) (string, bool) {
	switch {
	case o.Result == ResultRejected && o.Verdict == CauseRolledBack:
		return CauseRolledBack, true
	case o.Result == ResultRejected:
		return CauseRejected, true
	case !IsSelfInitiated(o.Family):
		return "", false
	case o.Verdict == CauseRegressed:
		return CauseRegressed, true
	case o.Result == ResultHarmful || o.Result == ResultSafetyViolation:
		return CauseHarmful, true
	}
	return "", false
}

// demoteForDemerit lowers o's pair one level when the demerit is news: a
// stored level set before the demerit was observed (an outcome without an
// observation time is news). A carried-over incident level is granted by
// policy and is not demoted automatically (M7 decision).
func (s *Service) demoteForDemerit(ctx context.Context, o Outcome, cause string) (
	*Demotion, error) {
	d, err := s.demoteOnce(ctx, o, cause)
	if errors.Is(err, ErrConflict) {
		d, err = s.demoteOnce(ctx, o, cause)
	}
	s.invalidate()
	return d, err
}

func (s *Service) demoteOnce(ctx context.Context, o Outcome, cause string) (*Demotion,
	error) {
	var out *Demotion
	err := s.store.withTx(ctx, func(tx pgx.Tx) error {
		out = nil
		st, found, err := s.store.readLevel(ctx, tx, o.Family, o.Class)
		if err != nil || !found || st.Provenance == ProvenanceCarriedOver {
			return err
		}
		if o.ObservedAt != nil && !o.ObservedAt.After(st.ChangedAt) {
			return nil // known when the level was set
		}
		to := DemotionTarget(st.Level)
		if to >= st.Level {
			return nil
		}
		d := Demotion{Database: s.store.database, Family: o.Family, Class: o.Class,
			From: st.Level, To: to, Cause: cause, ActionLogID: o.ActionLogID,
			QueueID: o.QueueID, At: s.now()}
		d.Detail = demotionReason(d, o.Actor)
		if err := s.writeDemotion(ctx, tx, st, d); err != nil {
			return err
		}
		out = &d
		return nil
	})
	return out, err
}

func (s *Service) writeDemotion(ctx context.Context, tx pgx.Tx, st State, d Demotion) error {
	evidence, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode demotion evidence: %w", err)
	}
	reason := truncate(d.Detail, 1000)
	if _, err := s.store.writeLevel(ctx, tx, levelChange{Family: d.Family, Class: d.Class,
		To: d.To, ExpectVersion: st.Version, Actor: ActorPgSage, Reason: reason,
		Evidence: evidence, At: d.At}); err != nil {
		return err
	}
	if _, err := s.store.closePending(ctx, tx, d.Family, d.Class, StatusSuperseded,
		ActorPgSage, "superseded by a demotion: "+d.Cause, d.At); err != nil {
		return err
	}
	return s.store.appendEvent(ctx, tx, Event{Family: d.Family, Class: d.Class,
		Type: EventAutoDowngraded, From: levelPtr(d.From), To: levelPtr(d.To),
		Actor: ActorPgSage, Reason: reason, ActionLogID: d.ActionLogID, Evidence: evidence,
		At: d.At})
}

// demotionReason says what demoted the pair, in one line.
func demotionReason(d Demotion, actor string) string {
	what := fmt.Sprintf("action %d (%s)", d.ActionLogID, d.Class)
	switch d.Cause {
	case CauseRegressed:
		what = "regressed: " + what
	case CauseRolledBack:
		what = "rolled back by an operator: " + what
	case CauseRejected:
		what = fmt.Sprintf("rejected by an operator: queue item %d (%s)", d.QueueID, d.Class)
	default:
		what = fmt.Sprintf("harmful outcome recorded by %s for %s", actor, d.Class)
	}
	return fmt.Sprintf("%s on %s; %s -> %s", what, d.Database, d.From, d.To)
}
