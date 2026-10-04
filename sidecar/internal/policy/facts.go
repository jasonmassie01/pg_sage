package policy

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Binding facts (roadmap 2.3). An operator confirms a typed fact once
// ("this index is owned by the app's migrations", "these schemas are test
// fixtures", "this slot belongs to CDC"); from then on the gate blocks any
// request the fact binds, naming the fact. Facts only ever narrow: they are
// consulted after the hard stops and request validation, a binding always
// blocks, and with no binding the gate decides exactly as without facts.
const (
	// ReasonBoundByFact blocks a request a confirmed fact binds;
	// Decision.Detail names the fact, who confirmed it and when, and where
	// the work goes instead (a source fix, an alert, a window).
	ReasonBoundByFact Reason = "bound_by_fact"
	// ReasonFactsUnavailable fails closed when the confirmed facts cannot
	// be read: an unread fact cannot be shown not to bind.
	ReasonFactsUnavailable Reason = "facts_unavailable"
)

// FactBinding is one confirmed fact that binds a request.
type FactBinding struct {
	FactID  int64
	Type    string
	Subject string
	// Route is where the work goes instead: source_fix, alert, keep,
	// excluded or wait_for_window.
	Route string
	// Summary is the fact in words; Object the request's object it matched.
	Summary     string
	Object      string
	ConfirmedBy string
	ConfirmedAt time.Time
}

// FactBinder answers which confirmed facts bind a request. An error means
// the facts could not be read; the gate then fails closed.
type FactBinder interface {
	Bind(context.Context, ActionRequest) ([]FactBinding, error)
}

// factDecision consults the confirmed facts for a mutation. Read-only
// diagnostics and rollbacks of pg_sage's own changes (which restore what
// the owner had) are never bound.
func (gate *authorizationGate) factDecision(
	ctx context.Context, req ActionRequest,
) (Decision, bool) {
	if gate.config.Facts == nil || req.Rollback || requestIsReadOnly(req) {
		return Decision{}, false
	}
	bindings, err := gate.config.Facts.Bind(ctx, req)
	if err != nil {
		return decisionForRequest(req, blocked(ReasonFactsUnavailable,
			"read confirmed facts: "+err.Error())), true
	}
	if len(bindings) == 0 {
		return Decision{}, false
	}
	return decisionForRequest(req, blocked(ReasonBoundByFact, FactDetail(bindings))), true
}

// FactDetail names every binding fact: what it says, who confirmed it and
// when, and the route the work takes instead.
func FactDetail(bindings []FactBinding) string {
	parts := make([]string, 0, len(bindings))
	for _, b := range bindings {
		part := fmt.Sprintf("fact #%d: %s", b.FactID, b.Summary)
		if b.ConfirmedBy != "" {
			part += fmt.Sprintf(" (confirmed by %s on %s)", b.ConfirmedBy,
				b.ConfirmedAt.UTC().Format("2006-01-02"))
		}
		if b.Route != "" {
			part += "; route: " + b.Route
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " | ")
}
