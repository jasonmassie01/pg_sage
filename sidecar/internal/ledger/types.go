package ledger

import "time"

type Verdict string

const (
	VerdictExecute       Verdict = "execute"
	VerdictQueueApproval Verdict = "queue_approval"
	VerdictPark          Verdict = "parked"
	VerdictBlocked       Verdict = "blocked"
	VerdictObserveOnly   Verdict = "observe_only"
)

type DecisionInput struct {
	DatabaseID     *int
	Feature        string
	Intent         string
	Evidence       map[string]any
	ProposedSQL    string
	Verdict        Verdict
	Reason         string
	RiskTier       string
	PolicyVersion  int
	TargetObjects  []string
	EvidenceID     string
	DeadlineKind   string
	DeadlineHardAt *time.Time
}

type Decision struct {
	DecisionInput
	ID        int64
	CreatedAt time.Time
}

type AuditViolation struct {
	ActionID int64  `json:"action_id"`
	Kind     string `json:"kind"`
	Detail   string `json:"detail,omitempty"`
}

type AuditResult struct {
	OK         bool             `json:"ok"`
	Count      int              `json:"count"`
	Violations []AuditViolation `json:"violations"`
}
