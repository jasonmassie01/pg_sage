// Package outcomes is the outcome ledger (ux-02a): one row per change
// pg_sage made, joining what it did, why, what it expected, what the
// verifier measured, the verdict and whether the change can still be
// rolled back. It is a read model over sage.action_log, sage.action_outcome
// (lab-01's verdict record), sage.decision, sage.action_queue and
// sage.plan_step; it writes nothing. Every number is computed here, never
// in a client.
package outcomes

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// The window and page bounds. MaxSince is evid-01's 400-day retention.
const (
	DefaultSince = 24 * time.Hour
	MaxSince     = 9600 * time.Hour
	DefaultLimit = 500
	MaxLimit     = 10000
)

// Verdicts, in the plan engine's vocabulary (changeplan.Outcome) plus
// pending: no verdict yet.
const (
	VerdictPending      = "pending"
	VerdictImproved     = "improved"
	VerdictNoChange     = "no_change"
	VerdictRegressed    = "regressed"
	VerdictInconclusive = "inconclusive"
	VerdictUnverifiable = "unverifiable"
	VerdictExpired      = "expired"
)

var verdicts = []string{VerdictPending, VerdictImproved, VerdictNoChange,
	VerdictRegressed, VerdictInconclusive, VerdictUnverifiable, VerdictExpired}

// Verdicts lists every ledger verdict.
func Verdicts() []string { return append([]string(nil), verdicts...) }

// Rollback states. RollbackInProgress is a rollback still running: it is
// neither available (a second one must not start) nor done.
const (
	RollbackAvailable     = "available"
	RollbackRolledBack    = "rolled_back"
	RollbackFailed        = "rollback_failed"
	RollbackNotReversible = "not_reversible"
	RollbackInProgress    = "rolling_back"
)

// Origins: how the change came to run.
const (
	OriginPlan       = "plan"       // a step of an approved change plan
	OriginApproved   = "approved"   // a person approved it
	OriginAgent      = "agent"      // an agent principal ran it under its grant
	OriginAutonomous = "autonomous" // pg_sage ran it under policy and trust
)

// Effect is a relative change of one metric, in percent (negative is a
// decrease), rounded to one decimal place.
type Effect struct {
	Metric    string  `json:"metric,omitempty"`
	ChangePct float64 `json:"change_pct"`
	Source    string  `json:"source,omitempty"`
}

// Row is one change. Measured is nil, with MeasuredReason, when there is
// no measurement (not judged yet, or the target cannot be verified).
type Row struct {
	ActionID       int64     `json:"action_id"`
	Database       string    `json:"database"`
	ActionType     string    `json:"action_type"`
	What           string    `json:"what"`
	Why            string    `json:"why"`
	Origin         string    `json:"origin"`
	Expected       *Effect   `json:"expected"`
	Measured       *Effect   `json:"measured"`
	MeasuredReason string    `json:"measured_reason,omitempty"`
	Verdict        string    `json:"verdict"`
	RollbackState  string    `json:"rollback_state"`
	EvidenceID     string    `json:"evidence_id,omitempty"`
	FindingID      *int64    `json:"finding_id,omitempty"`
	PlanID         *int64    `json:"plan_id,omitempty"`
	PackHash       string    `json:"pack_hash,omitempty"`
	Tier           string    `json:"tier,omitempty"` // lab-03a: highest ladder tier passed
	SavedHours     *float64  `json:"saved_hours,omitempty"`
	ExecutedAt     time.Time `json:"executed_at"`
}

// Summary counts a window's rows. MeasuredShare is Measured / Changes to
// three places, and null (not 0) when there are no changes.
type Summary struct {
	Changes       int      `json:"changes"`
	Verified      int      `json:"verified"`
	Reverted      int      `json:"reverted"`
	Inconclusive  int      `json:"inconclusive"`
	Pending       int      `json:"pending"`
	Measured      int      `json:"measured"`
	MeasuredShare *float64 `json:"measured_share"`
}

// ReadError names a database whose ledger could not be read.
type ReadError struct {
	Database string `json:"database"`
	Error    string `json:"error"`
}

// Result is a ledger read. Truncated says the window held more than
// MaxLimit changes, so the summary covers the newest MaxLimit only.
type Result struct {
	Since     string      `json:"since"`
	Rows      []Row       `json:"rows"`
	Summary   Summary     `json:"summary"`
	Truncated bool        `json:"truncated"`
	Errors    []ReadError `json:"errors"`
}

var errSince = fmt.Errorf("since must be a duration from 1s to %s (the 400-day "+
	"retention), such as 24h or 168h", FormatSince(MaxSince))

// ParseSince parses a window such as 24h; "" is DefaultSince.
func ParseSince(raw string) (time.Duration, error) {
	if raw == "" {
		return DefaultSince, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < time.Second || d > MaxSince {
		return 0, errSince
	}
	return d, nil
}

// ParseVerdict checks a verdict filter; "" is every verdict.
func ParseVerdict(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	for _, v := range verdicts {
		if raw == v {
			return raw, nil
		}
	}
	return "", errors.New("verdict must be one of " + strings.Join(verdicts, ", "))
}

// ParseLimit parses a page size; "" is DefaultLimit.
func ParseLimit(raw string) (int, error) {
	if raw == "" {
		return DefaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > MaxLimit {
		return 0, fmt.Errorf("limit must be an integer from 1 to %d", MaxLimit)
	}
	return n, nil
}

// FormatSince renders a window as hours, such as 24h or 1.5h.
func FormatSince(d time.Duration) string {
	return strconv.FormatFloat(d.Hours(), 'f', -1, 64) + "h"
}

// round rounds v to places decimal places.
func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
