package agentguard

import (
	"fmt"
	"sort"
	"time"
)

// ReplicaReport is the kill's result on one standby. A configured replica
// is reached by its DSN; an unconfigured one is only seen in
// pg_stat_replication: it is named with the bound within which its agent
// sessions end (new logins already fail there: NOLOGIN replicates).
type ReplicaReport struct {
	Name               string        `json:"name"`
	Configured         bool          `json:"configured"`
	ApplicationName    string        `json:"application_name,omitempty"`
	ClientAddr         string        `json:"client_addr,omitempty"`
	State              string        `json:"state,omitempty"`
	SeenFrom           string        `json:"seen_from,omitempty"`
	BackendsTerminated int           `json:"backends_terminated"`
	LoginsBlocked      bool          `json:"logins_blocked"`
	Verified           bool          `json:"verified"`
	Bound              *SessionBound `json:"bound,omitempty"`
	Note               string        `json:"note,omitempty"`
	Error              string        `json:"error,omitempty"`
}

// DatabaseReport is the kill's result on one monitored database (§8.3).
type DatabaseReport struct {
	Name                string          `json:"name"`
	RolesDisabled       int             `json:"roles_disabled"`
	BackendsTerminated  int             `json:"backends_terminated"`
	StatementsCancelled int             `json:"statements_cancelled"`
	ApprovalsCancelled  int             `json:"approvals_cancelled"`
	Replicas            []ReplicaReport `json:"replicas"`
	Verified            bool            `json:"verified"`
	// ActionID is the audited action on this database; Direct marks the
	// out-of-gate fallback (§6.2.5), audited in the local log.
	ActionID int64  `json:"action_id,omitempty"`
	Direct   bool   `json:"direct,omitempty"`
	Error    string `json:"error,omitempty"`
}

// KillReport is a kill's or a freeze's result. Verified means no agent
// backend remains and new logins fail on the primary and every
// configured replica of every database; ControlError is a failure of the
// control-database steps (marking principals frozen, revoking tokens),
// which does not stop the per-database steps.
type KillReport struct {
	KillID             int64            `json:"kill_id,omitempty"`
	Scope              KillScope        `json:"scope"`
	ID                 string           `json:"id,omitempty"`
	Reason             string           `json:"reason"`
	Principals         []string         `json:"principals"`
	TokensRevoked      int64            `json:"tokens_revoked"`
	ApprovalsCancelled int              `json:"approvals_cancelled"`
	Databases          []DatabaseReport `json:"databases"`
	Verified           bool             `json:"verified"`
	ControlError       string           `json:"control_error,omitempty"`
	StartedAt          time.Time        `json:"started_at"`
	FinishedAt         time.Time        `json:"finished_at"`
}

// StandbyObservation is one pg_stat_replication row, seen from the
// primary or from a configured replica (a cascading standby).
type StandbyObservation struct {
	Source          string
	ApplicationName string
	ClientAddr      string
	State           string
}

// classifyStandbys reports the observed standbys that are not configured.
// A standby is configured when its application_name (the walreceiver's
// cluster_name) is a configured replica's name or cluster_name.
func classifyStandbys(observed []StandbyObservation, configured map[string]bool,
	bound SessionBound) []ReplicaReport {
	out := []ReplicaReport{}
	seen := map[string]bool{}
	for _, o := range observed {
		if configured[o.ApplicationName] {
			continue
		}
		key := o.ApplicationName + "|" + o.ClientAddr
		if seen[key] {
			continue
		}
		seen[key] = true
		b := bound
		name := o.ApplicationName
		if name == "" {
			name = "standby"
		}
		if o.ClientAddr != "" {
			name = fmt.Sprintf("%s@%s", name, o.ClientAddr)
		}
		out = append(out, ReplicaReport{Name: name, ApplicationName: o.ApplicationName,
			ClientAddr: o.ClientAddr, State: o.State, SeenFrom: o.Source, Bound: &b,
			Note: "not configured: new logins fail once NOLOGIN replays; open agent " +
				"sessions end within the bound"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
