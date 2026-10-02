package action

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/sre"
)

// Proposal is one durable action proposal of one investigation.
type Proposal struct {
	ID              sre.UUID                `json:"id"`
	Scope           sre.Scope               `json:"-"`
	InvestigationID sre.UUID                `json:"investigation_id"`
	Class           ActionClass             `json:"class"`
	Family          string                  `json:"family"`
	Node            string                  `json:"node"`
	State           ProposalState           `json:"state"`
	Reason          ActionReason            `json:"reason,omitempty"`
	Detail          string                  `json:"detail,omitempty"`
	SQL             string                  `json:"sql,omitempty"`
	EvidenceIDs     []sre.UUID              `json:"evidence_ids"`
	Target          *BackendTarget          `json:"target,omitempty"`
	Baseline        Baseline                `json:"baseline"`
	Contract        executor.RepairContract `json:"contract"`
	Policy          PolicyVerdict           `json:"policy"`
	QueueID         int                     `json:"queue_id,omitempty"`
	FindingID       int                     `json:"finding_id,omitempty"`
	ActionLogID     int64                   `json:"action_log_id,omitempty"`
	RequestedBy     string                  `json:"requested_by,omitempty"`
	RequestedAt     *time.Time              `json:"requested_at"`
	DecidedBy       int                     `json:"decided_by,omitempty"`
	DecidedAt       *time.Time              `json:"decided_at"`
	ExecutedAt      *time.Time              `json:"executed_at"`
	ExpiresAt       time.Time               `json:"expires_at"`
	Recovery        RecoveryRecord          `json:"recovery"`
	Version         int64                   `json:"version"`
	CreatedAt       time.Time               `json:"created_at"`
	UpdatedAt       time.Time               `json:"updated_at"`
}

// RecoverySample is one fresh observation of the recovery predicate.
type RecoverySample struct {
	ObservedAt        time.Time `json:"observed_at"`
	Status            string    `json:"status"`
	Usable            bool      `json:"usable"`
	Reason            string    `json:"reason,omitempty"`
	TargetPresent     bool      `json:"target_present"`
	TargetBlocking    int       `json:"target_blocking"`
	Waiting           int       `json:"waiting"`
	ActiveNotWaiting  int       `json:"active_not_waiting"`
	WaitersPresent    int       `json:"waiters_present"`
	WaitersProgressed int       `json:"waiters_progressed"`
}

// RecoveryRecord is a recovery verification: its samples and verdict.
type RecoveryRecord struct {
	State        RecoveryState    `json:"state"`
	Attribution  string           `json:"attribution,omitempty"`
	StartedAt    time.Time        `json:"started_at"`
	Deadline     time.Time        `json:"deadline"`
	NextSampleAt time.Time        `json:"next_sample_at"`
	Samples      []RecoverySample `json:"samples"`
	Verdict      string           `json:"verdict,omitempty"`
	DecidedAt    time.Time        `json:"decided_at"`
}

// maxStoredSamples bounds a stored recovery record.
const maxStoredSamples = 40

func (r *RecoveryRecord) add(s RecoverySample) {
	r.Samples = append(r.Samples, s)
	if n := len(r.Samples); n > maxStoredSamples {
		r.Samples = append([]RecoverySample(nil), r.Samples[n-maxStoredSamples:]...)
	}
}

// ApprovalView is the state of a proposal's item in the approval queue.
type ApprovalView struct {
	QueueID            int       `json:"queue_id"`
	Status             string    `json:"status"`
	DecidedBy          int       `json:"decided_by,omitempty"`
	Reason             string    `json:"reason,omitempty"`
	VerificationStatus string    `json:"verification_status,omitempty"`
	ExpiresAt          time.Time `json:"expires_at"`
}

// ProposalView is a proposal as surfaces show it: redacted, with its
// database and approval item.
type ProposalView struct {
	Proposal
	Database string        `json:"database"`
	Approval *ApprovalView `json:"approval"`
}

// cancelTitle names the proposed action.
func cancelTitle(pid int32) string {
	return fmt.Sprintf("Cancel blocking backend pid %d", pid)
}

// summary is the one-line description chat and the queue show.
func (p Proposal) summary(database string) string {
	if p.Target == nil {
		return fmt.Sprintf("%s: %s", database, p.Detail)
	}
	t := p.Target
	return fmt.Sprintf("%s: pid %d (user %s) blocks %d sessions (%s); identity "+
		"rechecked before the signal; mitigation only", database, t.PID, t.User,
		p.Baseline.Waiting, strings.ReplaceAll(p.Node, "_", " "))
}

// View renders a proposal with its approval item, redacted (CHECK-30).
func (a *ActionService) View(ctx context.Context, p Proposal) (ProposalView, error) {
	v := ProposalView{Proposal: p, Database: a.svc.Name()}
	if p.QueueID > 0 {
		v.Approval = &ApprovalView{QueueID: p.QueueID, Status: "unknown"}
		st, err := a.queue.Status(ctx, p.QueueID)
		if err == nil {
			v.Approval = &ApprovalView{QueueID: p.QueueID, Status: st.Status,
				DecidedBy: st.DecidedBy, Reason: st.Reason,
				VerificationStatus: st.VerificationStatus, ExpiresAt: st.ExpiresAt}
		} else {
			a.logFn("WARN", "sre: proposal %s: reading approval item %d failed: %v",
				p.ID, p.QueueID, err)
		}
	}
	var out ProposalView
	return out, sre.RedactInto(v, &out)
}

// Views renders an investigation's proposals.
func (a *ActionService) Views(ctx context.Context, invID sre.UUID) ([]ProposalView, error) {
	ps, err := a.ForInvestigation(ctx, invID)
	if err != nil {
		return nil, err
	}
	out := make([]ProposalView, 0, len(ps))
	for _, p := range ps {
		v, err := a.View(ctx, p)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
