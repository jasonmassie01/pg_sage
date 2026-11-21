// Package whynot answers "why has pg_sage not done this?" as one chain of
// ten links, from observing the database to the emergency stop (ux-08a).
// Each link passes, fails, is unknown or does not apply, says so in one
// plain sentence and carries at most one typed fix. The chain reads only:
// the caller gathers its Inputs (the API reads the collector status, the
// open findings, the gate's explanation and a bounded catalog probe), and
// Chain itself touches nothing. Unknown is a valid answer and never
// counts as a pass.
package whynot

import "time"

// Status is one link's state.
type Status string

// Link states.
const (
	Pass          Status = "pass"
	Fail          Status = "fail"
	Unknown       Status = "unknown"
	NotApplicable Status = "not_applicable"
)

// LinkName names one link of the chain.
type LinkName string

// The ten links, in the order the chain checks them.
const (
	LinkObserved      LinkName = "observed"
	LinkFinding       LinkName = "finding"
	LinkCandidate     LinkName = "candidate"
	LinkPolicy        LinkName = "policy"
	LinkWindow        LinkName = "window"
	LinkBudget        LinkName = "budget"
	LinkPrivilege     LinkName = "privilege"
	LinkTrust         LinkName = "trust"
	LinkVerification  LinkName = "verification_wait"
	LinkEmergencyStop LinkName = "emergency_stop"
)

// Links is every link in chain order.
func Links() []LinkName {
	return []LinkName{LinkObserved, LinkFinding, LinkCandidate, LinkPolicy, LinkWindow,
		LinkBudget, LinkPrivilege, LinkTrust, LinkVerification, LinkEmergencyStop}
}

// FixKind is how a fix is applied.
type FixKind string

// Fix kinds: SQL an operator runs, a config setting, a dashboard page,
// or a time to wait for.
const (
	FixGrantSQL  FixKind = "grant_sql"
	FixSetting   FixKind = "setting"
	FixUILink    FixKind = "ui_link"
	FixWaitUntil FixKind = "wait_until"
)

// Fix is one typed remedy. Value is the SQL, the setting ("key=value"),
// the dashboard hash link or the RFC 3339 time; Note says who runs it or
// what happens next.
type Fix struct {
	Kind  FixKind `json:"kind"`
	Value string  `json:"value"`
	Note  string  `json:"note,omitempty"`
}

// Link is one checked link.
type Link struct {
	Name     LinkName `json:"name"`
	Status   Status   `json:"status"`
	Sentence string   `json:"sentence"`
	Fix      *Fix     `json:"fix,omitempty"`
	// NoFix says why a failing link has no fix (the cause is not
	// something an operator changes, or it clears on its own).
	NoFix string `json:"no_fix,omitempty"`
	// Reason is the gate's ledger reason when the link came from it.
	Reason string `json:"reason,omitempty"`
	// Error is the error class of an unknown link; raw errors can carry
	// hosts and roles, so they are never served.
	Error     string `json:"error,omitempty"`
	FirstFail bool   `json:"first_fail"`
}

// Result is the whole chain. FirstFail is the link marked first (the
// emergency stop when it fails, else the earliest failing link); Failing
// lists every failing link with FirstFail first.
type Result struct {
	Links     []Link     `json:"links"`
	FirstFail LinkName   `json:"first_fail,omitempty"`
	Failing   []LinkName `json:"failing"`
}

// Observed is the collector's evidence for the database.
type Observed struct {
	LastSnapshot time.Time
	// Freshness is the fleet's collection freshness: fresh, stale or
	// unknown ("" reads as unknown).
	Freshness string
	// Interval is the collector's period; zero when none is set.
	Interval time.Duration
}

// Findings is what the open findings say for the question asked.
type Findings struct {
	// Open is the number of open findings matching the question.
	Open int
	// Category is the category asked about ("" for none).
	Category string
	// FindingID is the single finding asked about (0 for none).
	FindingID int64
	Err       error
}

// Verdict is the gate's explanation of one candidate.
type Verdict struct {
	ActionType string
	Decision   string
	Reason     string
	Detail     string
}

// Privileges is the bounded catalog probe of the monitored database.
type Privileges struct {
	Role string
	// StatStatements is whether the pg_stat_statements extension exists
	// in the database.
	StatStatements bool
	// QueryTextVisible is whether the role sees other roles' query text.
	QueryTextVisible bool
	// Replica is whether the database is in recovery (a read-only
	// standby).
	Replica bool
}

// Budget is a change budget's use in its window, when the caller read it.
type Budget struct {
	Used, Limit int64
	FreesAt     time.Time
}

// Window is the effective maintenance window, when the caller read it.
type Window struct {
	Open     bool
	NextOpen time.Time
}

// Stop is the emergency-stop latch of the database.
type Stop struct {
	Stopped bool
	Fleet   bool
	By      string
	At      time.Time
}

// Inputs is everything Chain reads. Pointer facts are nil when the caller
// could not or did not read them; the matching error makes the link
// unknown with that error's class.
type Inputs struct {
	Now      time.Time
	Database string
	Provider string
	Observed Observed
	Findings Findings
	// LLMEnabled is whether LLM-assisted analysis is on.
	LLMEnabled bool
	// Candidates is the number of action candidates the open findings
	// project to; CandidatesErr makes the candidate link unknown.
	Candidates    int
	CandidatesErr error
	Verdicts      []Verdict
	PolicyErr     error
	Window        *Window
	Budget        *Budget
	Privileges    *Privileges
	PrivilegesErr error
	// TrustLevel is the executor's effective trust level ("" unknown).
	TrustLevel string
	Stop       Stop
}
