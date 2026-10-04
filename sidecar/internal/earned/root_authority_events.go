package earned

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/modellift"
)

// Roadmap 2.4 owner addition A: a family earning or losing model-root
// authority is automatic but never silent. Each change is recorded in the
// ledger history (class model_root) with the report that decided it and
// told to the operator through the notifier. A grant is recorded before
// an investigation may use it; the periodic reconcile finds the rest (a
// newer report, the deciding report aging out, another build).

// ModelRootClass is the history's action class for model-root authority.
const ModelRootClass ActionClass = "model_root"

// Model-root authority history entries.
const (
	EventRootAuthorityGranted EventType = "root_authority_granted"
	EventRootAuthorityRevoked EventType = "root_authority_revoked"
)

// RootAuthorityChange is a family gaining or losing model-root authority.
type RootAuthorityChange struct {
	Database string `json:"database"`
	Family   Family `json:"family"`
	Granted  bool   `json:"granted"`
	Reason   string `json:"reason"`
	// ReportID is the bench report that decided the change ("" when the
	// family has no measurement that counts any more).
	ReportID  string        `json:"report_id,omitempty"`
	Authority RootAuthority `json:"authority"`
	At        time.Time     `json:"at"`
}

// RootAuthorityNotifier tells an operator about a model-root authority
// change.
type RootAuthorityNotifier interface {
	NotifyRootAuthority(ctx context.Context, c RootAuthorityChange) error
}

// WithRootAuthorityNotifier gives the ledger the operator's notification
// path for model-root authority changes. Without one the changes are
// still recorded in the history.
func (s *Service) WithRootAuthorityNotifier(n RootAuthorityNotifier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rootNotifier = n
}

func (s *Service) rootAuthorityNotifier() RootAuthorityNotifier {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rootNotifier
}

// rootAuthorityTransition is the history entry a family's current grant
// needs given its last model-root entry (nil: none, never granted).
func rootAuthorityTransition(last *Event, granted bool) (EventType, bool) {
	held := last != nil && last.Type == EventRootAuthorityGranted
	switch {
	case granted && !held:
		return EventRootAuthorityGranted, true
	case !granted && held:
		return EventRootAuthorityRevoked, true
	}
	return "", false
}

// rootAuthorityEvidence is what the history keeps of the decision.
type rootAuthorityEvidence struct {
	ReportID        string               `json:"report_id,omitempty"`
	Status          string               `json:"status"`
	LowerBound      *float64             `json:"lower_bound,omitempty"`
	Threshold       float64              `json:"threshold"`
	MinOverrides    int                  `json:"min_overrides"`
	Overrides       modellift.Proportion `json:"overrides"`
	OverridesNeeded int                  `json:"overrides_needed"`
}

// rootAuthorityEvent is the history entry recording a.
func rootAuthorityEvent(a RootAuthority, typ EventType, at time.Time) Event {
	ev := rootAuthorityEvidence{Status: a.Status, LowerBound: a.Rule.LowerBound,
		Threshold: modellift.MinOverrideLowerBound, MinOverrides: modellift.MinOverrides,
		OverridesNeeded: a.OverridesNeeded}
	if a.Report != nil {
		ev.ReportID = a.Report.ID
	}
	if a.Lift != nil {
		ev.Overrides = proportion(a.Lift.Overrides)
	}
	raw, err := json.Marshal(ev)
	if err != nil { // plain fields: cannot fail
		raw = []byte(`{}`)
	}
	return Event{Family: a.Family, Class: ModelRootClass, Type: typ, Actor: ActorPgSage,
		Reason: truncate(a.Reason, 2000), Evidence: raw, At: at}
}

// settled is a recorded change (nil: none) and why telling it failed.
type settled struct {
	change    *RootAuthorityChange
	notifyErr error
}

// settleRootAuthority records a's change, if it is one, and tells the
// operator. A failed record fails the call; a failed notification is
// returned in the result (the change stays recorded and is not retold).
func (s *Service) settleRootAuthority(ctx context.Context, a RootAuthority) (settled,
	error) {
	now := s.now()
	typ, recorded, err := s.store.recordRootAuthority(ctx, a.Family, a.Granted,
		func(t EventType) Event { return rootAuthorityEvent(a, t, now) })
	if err != nil || !recorded {
		return settled{}, err
	}
	c := &RootAuthorityChange{Database: s.store.database, Family: a.Family,
		Granted: typ == EventRootAuthorityGranted, Reason: a.Reason, Authority: a, At: now}
	if a.Report != nil {
		c.ReportID = a.Report.ID
	}
	out := settled{change: c}
	if n := s.rootAuthorityNotifier(); n != nil {
		if err := n.NotifyRootAuthority(ctx, *c); err != nil {
			out.notifyErr = fmt.Errorf("notify model-root authority of %s: %w", a.Family, err)
		}
	}
	return out, nil
}

// ReconcileRootAuthority records and tells every family's model-root
// authority change since the history's last entry for it. Notification
// failures are returned (joined) after every family is recorded.
func (s *Service) ReconcileRootAuthority(ctx context.Context) ([]RootAuthorityChange,
	error) {
	view, err := s.ModelLiftView(ctx)
	if err != nil {
		return nil, err
	}
	changes := []RootAuthorityChange{}
	var notifyErrs []error
	for _, a := range view.Families {
		got, err := s.settleRootAuthority(ctx, a)
		if err != nil {
			return changes, err
		}
		if got.change != nil {
			changes = append(changes, *got.change)
		}
		if got.notifyErr != nil {
			notifyErrs = append(notifyErrs, got.notifyErr)
		}
	}
	return changes, errors.Join(notifyErrs...)
}
