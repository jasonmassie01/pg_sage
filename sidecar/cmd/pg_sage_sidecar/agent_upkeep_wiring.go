package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/upkeep"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// Agent role upkeep (spec §6.4, §6.6, G1-01) as fleet-wide jobs on the
// leader, fenced by the governance lease: retired agents' roles are
// dropped after agents.roles.retire_grace_days, broker passwords rotate
// every agents.broker.rotation_days, and connected agent backends are
// checked every minute (violations are critical findings).

const (
	agentUpkeepFirstDelay     = 2 * time.Minute
	agentUpkeepInterval       = time.Hour
	agentBackendCheckInterval = time.Minute
	agentDriftInterval        = 24 * time.Hour
)

// agentUpkeepConfig maps the agents settings onto the jobs' schedule.
func agentUpkeepConfig(c *config.Config) upkeep.Config {
	uc := upkeep.ConfigFrom(c.Agents.Roles.RetireGraceDays, c.Agents.Broker.RotationDays)
	uc.Roles = agentRoleConfig(c)
	return uc
}

// agentRoleConfig is the role contracts' configuration: the kill switch's
// role bounds plus the broker rotation period.
func agentRoleConfig(c *config.Config) agentguard.RoleConfig {
	rc := killConfig(c).Roles
	rc.BrokerCredentialRotation = time.Duration(c.Agents.Broker.RotationDays) * 24 *
		time.Hour
	if rc.RoleChangeLockTimeout <= 0 {
		rc.RoleChangeLockTimeout = agentguard.DefaultRoleConfig().RoleChangeLockTimeout
	}
	return rc
}

// startAgentUpkeep starts the jobs; nil without a control database
// (agent governance is posture-only) or when the role manager cannot be
// built. Without the encryption key the retire and backend jobs still run;
// rotation reports encryption_key_required.
func startAgentUpkeep(ctx context.Context, mgr *fleet.DatabaseManager,
	control *pgxpool.Pool, lead governanceLeader) *upkeep.Runner {
	if cfg == nil || control == nil {
		return nil
	}
	kr, err := configSecretKeyring(ctx, control)
	if err != nil {
		logWarn("agents", "agent broker rotation cannot open credentials: %v", err)
	}
	roles, err := agentguard.NewRoleManager(agentguard.NewStore(control), kr,
		agentRoleConfig(cfg))
	if err != nil {
		logError("agents", "agent role upkeep unavailable: %v", err)
		return nil
	}
	r, err := upkeep.New(control, roles, killTargets(mgr), agentUpkeepConfig(cfg))
	if err != nil {
		logError("agents", "agent role upkeep unavailable: %v", err)
		return nil
	}
	go afterDelay(ctx, agentUpkeepFirstDelay, func() {
		every(ctx, agentUpkeepInterval, func(c context.Context) {
			runAgentRoleUpkeep(c, r, lead)
		})
	})
	go afterDelay(ctx, agentUpkeepFirstDelay, func() {
		every(ctx, agentBackendCheckInterval, func(c context.Context) {
			runAgentBackendCheck(c, r, lead)
		})
	})
	go afterDelay(ctx, agentUpkeepFirstDelay, func() {
		every(ctx, agentDriftInterval, func(c context.Context) {
			runAgentDrift(c, r, lead)
		})
	})
	return r
}

// upkeepFence is the lease a job writes under; ok is false on a follower.
func upkeepFence(lead governanceLeader, job string) (upkeep.Fence, bool) {
	f, ok := lead.fence(job)
	return upkeep.Fence{Scope: f.Scope, Holder: f.Holder, Epoch: f.Epoch}, ok
}

func runAgentRoleUpkeep(ctx context.Context, r *upkeep.Runner, lead governanceLeader) {
	jobs := []struct {
		name string
		run  func(context.Context, upkeep.Fence) (upkeep.Report, error)
	}{{upkeep.JobRetireGrace, r.DropRetired}, {upkeep.JobBrokerRotation, r.RotateBroker}}
	for _, job := range jobs {
		fence, ok := upkeepFence(lead, "agent role upkeep")
		if !ok {
			return
		}
		rep, err := job.run(ctx, fence)
		if errors.Is(err, upkeep.ErrFenced) {
			logWarn("agents", "agent %s stopped: the leader lease moved mid-pass", job.name)
			return
		}
		if err != nil {
			logWarn("agents", "agent %s failed: %v", job.name, err)
		}
		info, warn := upkeepLines(job.name, rep)
		for _, l := range info {
			logInfo("agents", "%s", l)
		}
		for _, l := range warn {
			logWarn("agents", "%s", l)
		}
	}
}

// upkeepLines are the log lines of one role job pass.
func upkeepLines(job string, rep upkeep.Report) (info, warn []string) {
	if n := len(rep.Done); n > 0 {
		info = append(info, fmt.Sprintf("agent %s: %d done", job, n))
	}
	for _, o := range rep.Skipped {
		warn = append(warn, fmt.Sprintf("agent %s of %s on %s skipped (%s): %s", job,
			o.PrincipalID, o.ClusterKey, o.Reason, o.Detail))
	}
	for _, o := range rep.Incomplete {
		warn = append(warn, fmt.Sprintf("agent %s of %s on %s: revoke_incomplete, "+
			"another grantor must revoke first: %s", job, o.PrincipalID, o.ClusterKey, o.Fix))
	}
	for _, o := range rep.Withheld {
		warn = append(warn, fmt.Sprintf("agent %s of %s on %s withheld by the gate (%s); "+
			"retried next pass", job, o.PrincipalID, o.ClusterKey, o.Reason))
	}
	for _, o := range rep.Failed {
		warn = append(warn, fmt.Sprintf("agent %s of %s on %s failed: %s", job,
			o.PrincipalID, o.ClusterKey, o.Detail))
	}
	return info, warn
}

// agentBackendNotes remembers the logged violations so a minute-by-minute
// check logs a change, not every pass.
var agentBackendNotes backendNotes

func runAgentBackendCheck(ctx context.Context, r *upkeep.Runner, lead governanceLeader) {
	fence, ok := upkeepFence(lead, "agent backend check")
	if !ok {
		return
	}
	rep, err := r.CheckBackends(ctx, fence)
	if errors.Is(err, upkeep.ErrFenced) {
		return
	}
	if err != nil {
		logWarn("agents", "agent backend check failed: %v", err)
		return
	}
	for _, l := range agentBackendNotes.changes(rep) {
		logError("agents", "%s", l)
	}
}

// backendNotes is the last logged violation line per cluster and role.
type backendNotes struct {
	mu   sync.Mutex
	last map[string]string
}

// changes returns a line for each violation that is new or changed, and
// for each that cleared, since the previous pass.
func (n *backendNotes) changes(rep upkeep.BackendReport) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := map[string]string{}
	for _, v := range rep.Violations {
		for _, f := range v.Findings {
			now[v.ClusterKey+"/"+f.Role] = fmt.Sprintf("agent role %s on cluster %s is "+
				"connected and %s (G1-01, critical finding raised)", f.Role, v.ClusterKey,
				strings.Join(f.Problems, ", "))
		}
	}
	var out []string
	for k, line := range now {
		if n.last[k] != line {
			out = append(out, line)
		}
	}
	for k := range n.last {
		if _, still := now[k]; !still {
			out = append(out, "agent role "+k+" no longer violates G1-01; finding resolved")
		}
	}
	sort.Strings(out)
	n.last = now
	return out
}

func runAgentDrift(ctx context.Context, r *upkeep.Runner, lead governanceLeader) {
	fence, ok := upkeepFence(lead, "agent role drift")
	if !ok {
		return
	}
	rep, err := r.ReconcileDrift(ctx, fence)
	if errors.Is(err, upkeep.ErrFenced) {
		logWarn("agents", "agent role drift stopped: the leader lease moved mid-pass")
		return
	}
	if err != nil {
		logWarn("agents", "agent role drift failed: %v", err)
		return
	}
	for _, l := range driftLines(rep) {
		logWarn("agents", "%s", l)
	}
}

// driftLines are the log lines of one drift pass.
func driftLines(rep upkeep.DriftReport) []string {
	var out []string
	for _, d := range rep.Drift {
		line := fmt.Sprintf("agent role %s in %s drifted: %s", d.Role, d.Database,
			strings.Join(append(append([]string{}, d.Widening...), d.Narrowing...), "; "))
		if len(d.Corrected) > 0 {
			line += fmt.Sprintf(" (corrected: %s)", strings.Join(d.Corrected, "; "))
		}
		out = append(out, line)
	}
	keys := make([]string, 0, len(rep.Failed))
	for k := range rep.Failed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, fmt.Sprintf("agent role drift of %s not checked: %s", k,
			rep.Failed[k]))
	}
	return out
}
