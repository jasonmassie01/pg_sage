package cases

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/backendguard"
	"github.com/pg-sage/sidecar/internal/sanitize"
	"github.com/pg-sage/sidecar/internal/signalid"
)

// IncidentBlocker is the structured identity RCA records for one root
// blocker of a lock_contention incident (rca.BlockerIdentity): pid plus
// backend_start (PIDs are reused) plus query_start and a hash of the first
// 200 characters of the query. It is the only basis for a backend signal
// candidate; a PID found in incident prose never is.
type IncidentBlocker struct {
	PID            int       `json:"pid"`
	BackendStart   time.Time `json:"backend_start"`
	QueryStart     time.Time `json:"query_start"`
	QuerySHA       string    `json:"query_sha"`
	State          string    `json:"state"`
	TotalBlocked   int       `json:"total_blocked,omitempty"`
	LockedRelation string    `json:"locked_relation,omitempty"`
	// QueuedDDLPID is the in-flight DDL the queue waits behind; such a
	// blocker gets no signal (migration-05).
	QueuedDDLPID int `json:"queued_ddl_pid,omitempty"`
}

const (
	noStructuredBlockerReason = "no single structured blocker identity"
	noBackendIdentityReason   = "no structured backend identity"
	idleCancelReason          = "blocker is idle in transaction: pg_cancel_backend " +
		"does not end an idle session"
	queuedDDLReason = "sessions queue behind in-flight DDL pid %d, not this blocker: " +
		"cancel the DDL from its migration_safety finding (approval-only)"
	stateActive        = "active"
	stateIdleInTx      = "idle in transaction"
	signalScriptHeader = "/* pg_sage guarded %s: signals only the session recorded by " +
		"incident evidence; zero rows means it changed, so re-diagnose */\n"
)

// querySHAPattern is rca's query_sha: 8 bytes of sha256, lower-case hex.
var querySHAPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// lockBlockerCandidates builds the cancel/terminate candidates of a lock
// playbook. Only a lock_contention incident with exactly one complete root
// blocker identity gets a signal; the signal is a guarded script (never
// queued: the executor re-checks query text this evidence does not keep).
func lockBlockerCandidates(i SourceIncident, expires time.Time) []ActionCandidate {
	b, ok := singleRootBlocker(i)
	if !ok {
		return []ActionCandidate{blockedSignalCandidate(
			"cancel_backend", noStructuredBlockerReason, expires, i.Confidence)}
	}
	if b.QueuedDDLPID > 0 {
		reason := fmt.Sprintf(queuedDDLReason, b.QueuedDDLPID)
		return []ActionCandidate{blockedSignalCandidate(
			"cancel_backend", reason, expires, i.Confidence)}
	}
	switch b.State {
	case stateActive:
		out := []ActionCandidate{guardedSignalCandidate("cancel_backend", b, expires,
			i.Confidence)}
		if i.Severity == SeverityCritical {
			out = append(out, guardedSignalCandidate("terminate_backend", b, expires,
				i.Confidence))
		}
		return out
	case stateIdleInTx:
		return []ActionCandidate{
			blockedSignalCandidate("cancel_backend", idleCancelReason, expires, i.Confidence),
			guardedSignalCandidate("terminate_backend", b, expires, i.Confidence),
		}
	default:
		reason := fmt.Sprintf("blocker state %q has no guarded signal", b.State)
		return []ActionCandidate{blockedSignalCandidate(
			"cancel_backend", reason, expires, i.Confidence)}
	}
}

// singleRootBlocker returns the one complete blocker identity carried by
// the incident's lock_contention links. Zero, several or an incomplete
// identity yield false.
func singleRootBlocker(i SourceIncident) (IncidentBlocker, bool) {
	var found []IncidentBlocker
	for _, link := range i.CausalChain {
		if link.Signal == signalid.LockContention && link.Blocker != nil {
			found = append(found, *link.Blocker)
		}
	}
	if len(found) != 1 {
		return IncidentBlocker{}, false
	}
	b := found[0]
	if b.PID <= 0 || b.BackendStart.IsZero() || b.QueryStart.IsZero() ||
		!querySHAPattern.MatchString(b.QuerySHA) {
		return IncidentBlocker{}, false
	}
	return b, true
}

func blockedSignalCandidate(
	actionType, reason string,
	expires time.Time,
	confidence float64,
) ActionCandidate {
	c := signalCandidateBase(actionType, expires, confidence)
	c.BlockedReason = reason
	return c
}

func guardedSignalCandidate(
	actionType string,
	b IncidentBlocker,
	expires time.Time,
	confidence float64,
) ActionCandidate {
	c := signalCandidateBase(actionType, expires, confidence)
	c.ProposedSQL = guardedSignalSQL(actionType == "terminate_backend", b)
	identity := b
	c.BackendIdentity = &identity
	return c
}

func signalCandidateBase(
	actionType string,
	expires time.Time,
	confidence float64,
) ActionCandidate {
	c := ActionCandidate{
		ActionType:       actionType,
		RiskTier:         "moderate",
		Confidence:       confidence,
		ExpiresAt:        &expires,
		OutputModes:      []string{"script"},
		RollbackClass:    "not_reversible",
		VerificationPlan: []string{"refresh lock graph; verify waiters are no longer blocked"},
	}
	if actionType == "terminate_backend" {
		c.RiskTier = "high"
		c.VerificationPlan = []string{"verify the recorded session is gone and " +
			"waiters are no longer blocked"}
	}
	return c
}

// guardedSignalSQL renders a script that signals the recorded session only
// while it is still the same backend (pid + backend_start), on the same
// statement (query_start + query hash) and in the recorded state, and is
// not a protected session (backendguard).
func guardedSignalSQL(terminate bool, b IncidentBlocker) string {
	fn, op, from := "pg_cancel_backend", "cancel", "\n  FROM pg_stat_activity AS a"
	guards := ""
	if terminate {
		fn, op = "pg_terminate_backend", "terminate"
		from += backendguard.TerminateJoin
		guards = backendguard.TerminatePredicate
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, signalScriptHeader, op)
	fmt.Fprintf(&sb, "SELECT %s(a.pid) AS signalled, a.pid", fn)
	sb.WriteString(from)
	fmt.Fprintf(&sb, "\n WHERE a.pid = %d", b.PID)
	sb.WriteString("\n   AND a.backend_start = " + timestampLiteral(b.BackendStart))
	sb.WriteString("\n   AND a.query_start = " + timestampLiteral(b.QueryStart))
	sb.WriteString("\n   AND left(encode(sha256(convert_to(left(a.query, 200), 'UTF8')), " +
		"'hex'), 16) = " + sanitize.QuoteLiteral(b.QuerySHA))
	sb.WriteString("\n   AND a.state = " + sanitize.QuoteLiteral(b.State))
	sb.WriteString(guards + backendguard.ProtectedPredicate + ";")
	return sb.String()
}

func timestampLiteral(t time.Time) string {
	return sanitize.QuoteLiteral(t.UTC().Format(time.RFC3339Nano)) + "::timestamptz"
}
