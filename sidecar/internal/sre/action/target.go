package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sampleTarget probes one session's identity now and says why an action
// may not signal it (gone, protected, another database, a replica).
func (a *ActionService) sampleTarget(ctx context.Context, pid int32,
	backendStart time.Time) (BackendTarget, *Ineligible) {
	res := a.targets.Run(ctx, probes.SignalTarget, probes.Args{PID: pid,
		BackendStart: backendStart})
	if !res.Status.Usable() {
		return BackendTarget{}, ineligible(ReasonTargetProbeFailed,
			"the target probe returned %s (%s)", res.Status, res.Reason)
	}
	rows, err := probes.SignalTargets(res)
	if err != nil {
		return BackendTarget{}, ineligible(ReasonTargetProbeFailed,
			"the target probe is unreadable: %v", err)
	}
	if len(rows) == 0 {
		return BackendTarget{}, ineligible(ReasonTargetGone,
			"pid %d (this session) is gone", pid)
	}
	r := rows[0]
	return BackendTarget{PID: r.PID, BackendStart: r.BackendStart,
		QueryStart: r.QueryStart, XactStart: r.XactStart, Database: r.Database,
		User: r.User, State: r.State, BackendType: r.BackendType,
		QueryHash: r.QueryHash, QueryID: r.QueryID, Blocking: r.Blocking,
		InRecovery: r.InRecovery, ObservedAt: res.ObservedAt,
		StatementIsTransaction: !r.XactStart.IsZero() && r.XactStart.Equal(r.QueryStart),
	}, a.protection(r)
}

// protection refuses sessions an action must never signal.
func (a *ActionService) protection(r probes.TargetRow) *Ineligible {
	switch {
	case !r.InCurrentDatabase:
		return ineligible(ReasonOtherDatabase, "pid %d belongs to another database", r.PID)
	case r.BackendType != "client backend":
		return ineligible(ReasonProtected, "pid %d is a %s, not a client backend",
			r.PID, r.BackendType)
	case r.ProtectedApplication:
		return ineligible(ReasonProtected,
			"pid %d is pg_sage or a dump/backup tool", r.PID)
	case containsString(a.cfg.ProtectedRoles, r.User):
		return ineligible(ReasonProtected, "role %s is protected by "+
			"sre.actions.protected_roles", r.User)
	case a.protectedApp(r.ApplicationHash):
		return ineligible(ReasonProtected, "pid %d's application is protected by "+
			"sre.actions.protected_applications", r.PID)
	case r.InRecovery:
		return ineligible(ReasonReplica, "the server is a replica: no mutation")
	}
	return nil
}

func (a *ActionService) protectedApp(hash string) bool {
	for _, name := range a.cfg.ProtectedApplications {
		sum := sha256.Sum256([]byte(name))
		if hex.EncodeToString(sum[:]) == hash {
			return true
		}
	}
	return false
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// liveRefusal rules out a protected, idle or non-blocking target.
func liveRefusal(t BackendTarget) *Ineligible {
	switch {
	case strings.HasPrefix(t.State, "idle in transaction"):
		return idleInTransaction(sessionName(t.PID))
	case t.State != "active":
		return ineligible(ReasonTargetNotActive, "pid %d is %q now: its statement "+
			"finished", t.PID, t.State)
	case t.Blocking == 0:
		return ineligible(ReasonNotBlocking, "pid %d no longer blocks anyone", t.PID)
	}
	return nil
}

// checkCandidate samples the derived target and checks it still is what
// the evidence showed: the same statement, active, blocking, signallable.
func (a *ActionService) checkCandidate(ctx context.Context,
	c cancelCandidate) (BackendTarget, *Ineligible) {
	t, why := a.sampleTarget(ctx, c.PID, c.BackendStart)
	if why != nil {
		return t, why
	}
	if c.QueryID != 0 && t.QueryID != c.QueryID {
		return t, ineligible(ReasonTargetChanged, "pid %d now runs another query "+
			"(query_id changed since the evidence)", c.PID)
	}
	return t, liveRefusal(t)
}

// recheck samples the approved target again right before the signal and
// compares the whole identity.
func (a *ActionService) recheck(ctx context.Context, want BackendTarget) (BackendTarget,
	[]string, *Ineligible) {
	t, why := a.sampleTarget(ctx, want.PID, want.BackendStart)
	if why != nil {
		return t, nil, why
	}
	if diff := identityDiff(want, t); len(diff) > 0 {
		return t, diff, ineligible(ReasonTargetChanged, "pid %d changed since the "+
			"approval (%s): a different statement", want.PID, strings.Join(diff, ", "))
	}
	return t, nil, liveRefusal(t)
}

// identityDiff names the identity fields that differ.
func identityDiff(want, got BackendTarget) []string {
	var out []string
	for name, same := range map[string]bool{
		"backend_start": want.BackendStart.Equal(got.BackendStart),
		"query_start":   want.QueryStart.Equal(got.QueryStart),
		"database":      want.Database == got.Database, "user": want.User == got.User,
		"query_hash": want.QueryHash == got.QueryHash, "query_id": want.QueryID == got.QueryID,
	} {
		if !same {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func sessionName(pid int32) string { return fmt.Sprintf("pid %d", pid) }
