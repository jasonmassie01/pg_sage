package shadow

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// Errors of the shadow ledger.
var (
	ErrInvalid     = errors.New("shadow: invalid request")
	ErrUnavailable = errors.New("shadow: no database pool")
	ErrNotFound    = errors.New("shadow: decision not found")
)

// Statuses: a decision waits for evidence (pending) until it is scored.
const (
	StatusPending = "pending"
	StatusScored  = "scored"
)

// Scores. Correct and incorrect follow the family's rule (tuning: only an
// improvement is correct; hygiene: a neutral that held is correct too);
// neutral is a decided outcome that earns nothing; unscored means no
// evidence could judge the decision.
const (
	ScoreCorrect   = "correct"
	ScoreIncorrect = "incorrect"
	ScoreNeutral   = "neutral"
	ScoreUnscored  = "unscored"
)

// Score sources, in precedence order. Operator and applied rest on an
// action the trust ledger already counts as a real outcome; external and
// hypopg are evidence only shadow mode produces, the ones it counts.
const (
	SourceOperator = "operator" // the operator's decision on the same proposal
	SourceApplied  = "applied"  // the same change, applied later through pg_sage
	SourceExternal = "external" // the same change, applied outside pg_sage
	SourceHypoPG   = "hypopg"   // HypoPG what-if (index creates)
	SourceNone     = "none"     // nothing applied before the horizon
)

// Decision is one shadow decision: what pg_sage would have done for a
// finding below its class's earned level, and how it scored.
type Decision struct {
	ID               int64             `json:"id"`
	DatabaseID       *int              `json:"database_id,omitempty"`
	Database         string            `json:"database"`
	Fingerprint      string            `json:"fingerprint"`
	Family           string            `json:"family"`
	Class            string            `json:"class"`
	FindingID        int64             `json:"finding_id,omitempty"`
	RecommendationID int64             `json:"recommendation_id,omitempty"`
	Title            string            `json:"title"`
	Object           string            `json:"object,omitempty"`
	SQL              string            `json:"sql"`
	RollbackSQL      string            `json:"rollback_sql,omitempty"`
	Shape            string            `json:"shape"`
	Prediction       verify.Prediction `json:"prediction"`
	Evidence         map[string]any    `json:"evidence"`
	GateVerdict      string            `json:"gate_verdict"`
	GateReason       string            `json:"gate_reason"`
	TrustedVerdict   string            `json:"trusted_verdict"`
	TrustedReason    string            `json:"trusted_reason"`
	TrustedDetail    string            `json:"trusted_detail,omitempty"`
	GrantedLevel     int               `json:"granted_level"`
	DecisionID       int64             `json:"decision_id,omitempty"`

	Status         string         `json:"status"`
	Score          string         `json:"score,omitempty"`
	ScoreSource    string         `json:"score_source,omitempty"`
	Counted        bool           `json:"counted"`
	ScoreReason    string         `json:"score_reason,omitempty"`
	ScoreDetail    map[string]any `json:"score_detail,omitempty"`
	RefActionLogID int64          `json:"ref_action_log_id,omitempty"`
	RefQueueID     int64          `json:"ref_queue_id,omitempty"`

	AppliedAfter      *time.Time `json:"applied_after,omitempty"`
	AppliedDetectedAt *time.Time `json:"applied_detected_at,omitempty"`
	SeenCount         int        `json:"seen_count"`
	RecordedAt        time.Time  `json:"recorded_at"`
	LastSeenAt        time.Time  `json:"last_seen_at"`
	ScoredAt          *time.Time `json:"scored_at,omitempty"`
}

var (
	gateVerdicts    = []string{"observe_only", "queue_approval", "blocked"}
	trustedVerdicts = []string{"execute", "queue_approval", "observe_only", "blocked"}
	statuses        = []string{StatusPending, StatusScored}
	scores          = []string{ScoreCorrect, ScoreIncorrect, ScoreNeutral, ScoreUnscored}
)

func oneOf(v string, allowed []string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// validate refuses a decision the ledger cannot hold.
func (d Decision) validate() error {
	switch {
	case d.Fingerprint == "" || len(d.Fingerprint) > 128:
		return fmt.Errorf("%w: a fingerprint of 1..128 characters", ErrInvalid)
	case !classPattern.MatchString(d.Class) || !classPattern.MatchString(d.Family):
		return fmt.Errorf("%w: family %q / class %q", ErrInvalid, d.Family, d.Class)
	case strings.TrimSpace(d.SQL) == "":
		return fmt.Errorf("%w: no SQL", ErrInvalid)
	case !oneOf(d.GateVerdict, gateVerdicts):
		return fmt.Errorf("%w: gate verdict %q is never shadowed", ErrInvalid, d.GateVerdict)
	case !oneOf(d.TrustedVerdict, trustedVerdicts):
		return fmt.Errorf("%w: trusted verdict %q", ErrInvalid, d.TrustedVerdict)
	case d.GrantedLevel < 0 || d.GrantedLevel > 4:
		return fmt.Errorf("%w: granted level %d", ErrInvalid, d.GrantedLevel)
	case len(d.Database) > 200:
		return fmt.Errorf("%w: database name too long", ErrInvalid)
	}
	return nil
}
