package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Reading the ledger (policy-07). A withheld verdict (queued, parked,
// blocked, observe-only) stays open until its candidate executes or a
// queue item takes it over; ListOpenWithheld answers "why hasn't this
// run?" from those open rows, one entry per candidate.

// Limits of one ListOpenWithheld read.
const (
	DefaultWithheldLimit = 100
	MaxWithheldLimit     = 500
)

// ErrInvalidFilter rejects a filter before any SQL runs.
var ErrInvalidFilter = errors.New("invalid decision filter")

// WithheldFilter selects open withheld decisions. Verdict, Reason and
// Since apply to an entry's latest state.
type WithheldFilter struct {
	// DatabaseID, when set, reads only that database's rows; nil reads
	// every row of the pool (one monitored database's ledger).
	DatabaseID *int64
	Verdict    Verdict
	Reason     string
	// Since keeps entries last seen at or after it.
	Since *time.Time
	Limit int
}

// IsWithheldVerdict reports a verdict that leaves the action unrun.
func IsWithheldVerdict(verdict Verdict) bool {
	switch verdict {
	case VerdictQueueApproval, VerdictPark, VerdictBlocked, VerdictObserveOnly:
		return true
	}
	return false
}

// Validate checks the verdict and the limit (1 to MaxWithheldLimit). The
// reason is the caller's to check: the reasons are the policy package's.
func (f WithheldFilter) Validate() error {
	if f.Verdict != "" && !IsWithheldVerdict(f.Verdict) {
		return fmt.Errorf("%w: verdict %q is not one of queue_approval, parked, "+
			"blocked, observe_only", ErrInvalidFilter, f.Verdict)
	}
	if f.Limit < 1 || f.Limit > MaxWithheldLimit {
		return fmt.Errorf("%w: limit %d is outside 1-%d", ErrInvalidFilter, f.Limit,
			MaxWithheldLimit)
	}
	return nil
}

// WithheldEntry is one candidate's open withheld decisions: the latest
// row's verdict, reason and evidence, every row's repeats. It carries no
// SQL text.
type WithheldEntry struct {
	DecisionID    int64    `json:"decision_id"`
	CandidateKey  string   `json:"candidate_key"`
	Verdict       Verdict  `json:"verdict"`
	Reason        string   `json:"reason"`
	RiskTier      string   `json:"risk_tier"`
	Feature       string   `json:"feature"`
	Intent        string   `json:"intent"`
	TargetObjects []string `json:"target_objects"`
	// RepeatCount is every evaluation the open rows counted; OpenRows is
	// how many open rows (one per verdict and policy version) they are.
	RepeatCount            int64           `json:"repeat_count"`
	OpenRows               int             `json:"open_rows"`
	FirstSeen              time.Time       `json:"first_seen"`
	LastSeenAt             time.Time       `json:"last_seen_at"`
	BudgetDetail           string          `json:"budget_detail,omitempty"`
	VerificationWait       json.RawMessage `json:"verification_wait,omitempty"`
	VerificationWaitDetail string          `json:"verification_wait_detail,omitempty"`
	DeadlineKind           string          `json:"deadline_kind,omitempty"`
	DeadlineHardAt         *time.Time      `json:"deadline_hard_at,omitempty"`
	PolicyVersion          *int            `json:"policy_version"`
	PolicySHA256           string          `json:"policy_doc_sha256,omitempty"`
	QueueID                *int            `json:"queue_id,omitempty"`
	FindingID              string          `json:"finding_id,omitempty"`
	PrincipalID            string          `json:"principal_id,omitempty"`
	EvidenceID             string          `json:"evidence_id"`
}

// RowQuerier runs a read: a pool, a connection or a transaction.
type RowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ListOpenWithheld returns up to filter.Limit entries, newest first, and
// whether more matched. It records nothing.
func ListOpenWithheld(
	ctx context.Context, q RowQuerier, filter WithheldFilter,
) ([]WithheldEntry, bool, error) {
	if err := filter.Validate(); err != nil {
		return nil, false, err
	}
	if q == nil {
		return nil, false, ErrRepositoryUnavailable
	}
	sql, args := withheldQuery(filter)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, false, fmt.Errorf("read open withheld decisions: %w", err)
	}
	defer rows.Close()
	entries := make([]WithheldEntry, 0)
	for rows.Next() {
		entry, err := scanWithheld(rows)
		if err != nil {
			return nil, false, fmt.Errorf("scan open withheld decisions: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("read open withheld decisions: %w", err)
	}
	if len(entries) > filter.Limit {
		return entries[:filter.Limit], true, nil
	}
	return entries, false, nil
}

func scanWithheld(rows pgx.Rows) (WithheldEntry, error) {
	var e WithheldEntry
	var targets []byte
	var wait []byte
	var key, budget, waitDetail, deadlineKind, hash, finding, principal *string
	err := rows.Scan(&e.DecisionID, &key, &e.Verdict, &e.Reason, &e.RiskTier, &e.Feature,
		&e.Intent, &targets, &e.RepeatCount, &e.OpenRows, &e.FirstSeen, &e.LastSeenAt,
		&budget, &wait, &waitDetail, &deadlineKind, &e.DeadlineHardAt, &e.PolicyVersion,
		&hash, &e.QueueID, &finding, &principal, &e.EvidenceID)
	if err != nil {
		return e, err
	}
	e.TargetObjects = []string{}
	if err := json.Unmarshal(targets, &e.TargetObjects); err != nil {
		return e, fmt.Errorf("decision %d target_objects: %w", e.DecisionID, err)
	}
	if len(wait) > 0 {
		e.VerificationWait = json.RawMessage(wait)
	}
	e.CandidateKey, e.BudgetDetail = stringOrEmpty(key), stringOrEmpty(budget)
	e.VerificationWaitDetail = stringOrEmpty(waitDetail)
	e.DeadlineKind, e.PolicySHA256 = stringOrEmpty(deadlineKind), stringOrEmpty(hash)
	e.FindingID, e.PrincipalID = stringOrEmpty(finding), stringOrEmpty(principal)
	return e, nil
}

func stringOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
