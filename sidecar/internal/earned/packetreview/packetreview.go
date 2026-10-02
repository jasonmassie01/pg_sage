// Package packetreview records an operator's review of a finished Sage
// SRE investigation (2026-10-02 roadmap Phase 1.1). One review writes
// both records that describe the investigation, so they never disagree:
// the shadow review behind earned autonomy (accepted / rejected, in the
// database's ledger) and the investigation outcome in incident memory
// (confirmed / refuted, with the actual root node when the operator names
// a causal-graph node). A free-text root cause stays in the review note.
// The API review route, the investigation outcome route and the MCP tool
// all go through Record. It is separate from internal/earned because the
// executor imports the ledger and must not import internal/sre.
package packetreview

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/causal"
)

// Review limits: the note and the root cause together fit the ledger's
// 2000-character note.
const (
	maxNote         = 1500
	maxRootCause    = 400
	maxOutcomeActor = 128
)

var (
	// ErrNotFinished: only a concluded or inconclusive investigation is
	// reviewed.
	ErrNotFinished = errors.New("only a concluded or inconclusive investigation can be " +
		"reviewed")
	// ErrUnavailable: the database has no ledger or no investigations.
	ErrUnavailable = errors.New("investigation review unavailable")
)

// Investigations is one database's investigation service.
type Investigations interface {
	Detail(ctx context.Context, id sre.UUID) (sre.Detail, error)
	RecordOutcome(ctx context.Context, id sre.UUID, req sre.OutcomeRequest) (sre.Outcome,
		error)
}

// Ledger is one database's earned-autonomy ledger.
type Ledger interface {
	RecordReview(ctx context.Context, r earned.Review) error
	Database() string
}

// Request is one review.
type Request struct {
	InvestigationID string
	// Verdict is earned.VerdictAccepted or earned.VerdictRejected.
	Verdict string
	Note    string
	// ActualRootCause is a causal-graph node id or free text.
	ActualRootCause string
	Actor           string
	// RequireOutcome refuses, before writing anything, a review whose
	// investigation outcome cannot be recorded (the outcome route).
	RequireOutcome bool
}

// Result is what one review recorded.
type Result struct {
	Database   string       `json:"database"`
	Family     string       `json:"family"`
	Verdict    string       `json:"verdict"`
	ActualNode string       `json:"actual_node,omitempty"`
	Outcome    *sre.Outcome `json:"investigation_outcome,omitempty"`
	// OutcomeSkipped says why no investigation outcome was recorded.
	OutcomeSkipped string `json:"investigation_outcome_skipped,omitempty"`
}

// Record writes the review to the ledger, then the investigation outcome.
// A failure of the outcome after the review is returned; repeating the
// review replaces the verdict and appends the outcome.
func Record(ctx context.Context, ledger Ledger, inv Investigations,
	req Request) (Result, error) {
	if ledger == nil || inv == nil {
		return Result{}, fmt.Errorf("%w: the database needs a ledger and investigations",
			ErrUnavailable)
	}
	if err := req.validate(); err != nil {
		return Result{}, err
	}
	d, err := inv.Detail(ctx, sre.UUID(req.InvestigationID))
	if err != nil {
		return Result{}, err
	}
	i := d.Investigation
	if i.State != sre.StateConcluded && i.State != sre.StateInconclusive {
		return Result{}, fmt.Errorf("%w (state %s)", ErrNotFinished, i.State)
	}
	res := Result{Database: ledger.Database(), Family: family(i), Verdict: req.Verdict,
		ActualNode: actualNode(req.ActualRootCause)}
	outcome, skipped := outcomeRequest(i, req, res.ActualNode)
	if skipped != "" && req.RequireOutcome {
		return Result{}, fmt.Errorf("%w: %s", earned.ErrInvalidRequest, skipped)
	}
	err = ledger.RecordReview(ctx, earned.Review{Database: res.Database,
		InvestigationID: req.InvestigationID, Family: earned.Family(res.Family),
		Verdict: req.Verdict, Reviewer: req.Actor, Note: note(req)})
	if err != nil {
		return Result{}, err
	}
	if skipped != "" {
		res.OutcomeSkipped = skipped
		return res, nil
	}
	o, err := inv.RecordOutcome(ctx, sre.UUID(req.InvestigationID), outcome)
	if err != nil {
		return res, fmt.Errorf("review recorded, investigation outcome failed: %w", err)
	}
	res.Outcome = &o
	return res, nil
}

func (r Request) validate() error {
	switch {
	case r.Verdict != earned.VerdictAccepted && r.Verdict != earned.VerdictRejected:
		return fmt.Errorf("%w: verdict must be accepted or rejected", earned.ErrInvalidRequest)
	case !validID(r.InvestigationID):
		return fmt.Errorf("%w: investigation id", earned.ErrInvalidRequest)
	case strings.TrimSpace(r.Actor) == "":
		return fmt.Errorf("%w: a review needs a reviewer", earned.ErrInvalidRequest)
	case len(r.Note) > maxNote:
		return fmt.Errorf("%w: note longer than %d characters", earned.ErrInvalidRequest,
			maxNote)
	case len(r.ActualRootCause) > maxRootCause:
		return fmt.Errorf("%w: actual root cause longer than %d characters",
			earned.ErrInvalidRequest, maxRootCause)
	}
	return nil
}

func validID(id string) bool {
	_, err := sre.ParseUUID(id)
	return err == nil
}

// family is the investigation's incident family (its trigger kind when
// the summary has none), as the ledger keys it.
func family(i sre.Investigation) string {
	if i.Summary.Family != "" {
		return i.Summary.Family
	}
	return string(i.TriggerKind)
}

// actualNode is the root cause when it names a causal-graph node.
func actualNode(rootCause string) string {
	id := strings.TrimSpace(rootCause)
	if _, ok := causal.NodeByID(causal.NodeID(id)); ok && id != "" {
		return id
	}
	return ""
}

// outcomeRequest maps the review to the investigation outcome, or says
// why none can be recorded.
func outcomeRequest(i sre.Investigation, req Request, node string) (sre.OutcomeRequest,
	string) {
	text := strings.TrimSpace(req.ActualRootCause)
	switch {
	case text != "" && node == "" && req.RequireOutcome:
		return sre.OutcomeRequest{}, fmt.Sprintf("%q is not a causal graph node", text)
	case req.Verdict == earned.VerdictAccepted && i.Summary.Root == "" && node == "":
		return sre.OutcomeRequest{}, "an inconclusive investigation is confirmed only " +
			"with a graph node as the actual root cause"
	}
	verdict := sre.OutcomeConfirmed
	if req.Verdict == earned.VerdictRejected {
		verdict = sre.OutcomeRefuted
	}
	actor := req.Actor
	if len(actor) > maxOutcomeActor {
		actor = actor[:maxOutcomeActor]
	}
	return sre.OutcomeRequest{Verdict: string(verdict), ActualNode: node, Actor: actor}, ""
}

// note is the review note with the free-text root cause.
func note(req Request) string {
	n, root := strings.TrimSpace(req.Note), strings.TrimSpace(req.ActualRootCause)
	if root == "" {
		return n
	}
	if n == "" {
		return "actual root cause: " + root
	}
	return n + "\nactual root cause: " + root
}
