package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sreActionPoll is how often the action service syncs approval decisions,
// takes due recovery samples and proposes for concluded investigations.
const sreActionPoll = 5 * time.Second

// sreActionDeps is one database's Sage SRE action wiring (M5).
type sreActionDeps struct {
	readPolicy *probes.ReadPolicy
	service    *sre.Service
	monitored  *pgxpool.Pool
	executor   *executor.Executor
	// cards is the database's approval-card notifier (executorDispatcher);
	// nil when notifications are off.
	cards      executor.EventDispatcher
	databaseID *int
	name       string
	settings   config.SREActionsConfig
	logFn      func(string, string, ...any)
}

// newSREActions builds a database's action service and registers it as
// the executor's runner for its approved items: proposals queue in the
// database's existing approval flow and run only after a human approval.
func newSREActions(d sreActionDeps) (*sreaction.ActionService, error) {
	if d.service == nil || d.monitored == nil || d.executor == nil {
		return nil, fmt.Errorf("db %q: sre actions need the investigator, the "+
			"monitored database and its executor", d.name)
	}
	var notifier sreaction.ApprovalNotifier
	if d.cards != nil {
		notifier = dispatcherNotifier{d: d.cards}
	}
	targets := probes.NewRunner(d.monitored, probes.ActionRegistry(), sreProbeLimiter,
		d.readPolicy)
	targets.SetDatabaseName(d.name)
	a, err := sreaction.NewActionService(sreaction.ActionDeps{Service: d.service,
		Targets:  targets,
		Queue:    sreaction.NewPGApprovalQueue(d.monitored, d.databaseID),
		Executor: d.executor, Notifier: notifier, Config: sreActionConfig(d.settings),
		LogFn: d.logFn})
	if err != nil {
		return nil, err
	}
	// Maintenance retention joins its SRE ledger to the target's approval and
	// rollback records. A split control store cannot provide those local holds.
	colocatedMaintenance := probes.PoolBinding(d.service.Store().Pool()) == probes.PoolBinding(d.monitored)
	if d.readPolicy.Allows(probes.StatTables) && !colocatedMaintenance && d.logFn != nil {
		d.logFn("WARN", "sre: db %q: operator maintenance unavailable with a separate coordination database", d.name)
	}
	if d.readPolicy.Allows(probes.StatTables) && colocatedMaintenance {
		ttl := d.settings.ApprovalTTL()
		if ttl > 15*time.Minute {
			ttl = 15 * time.Minute
		}
		m, err := sreaction.NewMaintenanceService(sreaction.MaintenanceDeps{Service: d.service,
			Pool: d.monitored, Executor: d.executor, DatabaseID: d.databaseID,
			// Backend-cancel evidence has a seconds-long freshness setting.
			// Maintenance uses its separate bounded 15-minute default.
			ApprovalTTL: ttl})
		if err != nil {
			return nil, err
		}
		a.SetMaintenanceService(m)
	}
	if err := d.executor.AddApprovedActionRunner(sreaction.RunnerName,
		sreaction.RunnerKeyPrefixes(), a); err != nil {
		return nil, err
	}
	return a, nil
}

func sreActionConfig(s config.SREActionsConfig) sreaction.ActionConfig {
	return sreaction.ActionConfig{Proposals: s.Proposals, RequestApproval: s.RequestApproval,
		ApprovalTTL: s.ApprovalTTL(), MaxEvidenceAge: s.MaxEvidenceAge(),
		RecoveryInterval: s.RecoveryInterval(), RecoverySamples: s.RecoverySamples,
		RecoveryDeadline: s.RecoveryDeadline(), PollInterval: sreActionPoll,
		ProtectedRoles:        s.ProtectedRoles,
		ProtectedApplications: s.ProtectedApplications}
}

// startSREActions runs the database's action service after its executor
// exists, in every mode.
func (rt *databaseRuntime) startSREActions() {
	policy := rt.sreReadPolicy()
	if !policy.Allows(probes.SignalTarget) && !policy.Allows(probes.StatTables) {
		logInfo(rt.spec.Scope, "db %q: SRE actions disabled by read scope", rt.spec.Name)
		return
	}
	if rt.sreService == nil || rt.executor == nil {
		return
	}
	var dbID *int
	if rt.spec.DatabaseID > 0 {
		id := rt.spec.DatabaseID
		dbID = &id
	}
	settings := rt.cfg.SRE.Actions
	if !policy.Allows(probes.SignalTarget) {
		settings.Proposals = false
	}
	a, err := newSREActions(sreActionDeps{service: rt.sreService, monitored: rt.spec.Pool, readPolicy: policy,
		executor: rt.executor, databaseID: dbID,
		cards: executorDispatcher(rt.dispatcher, rt.spec.Pool, rt.spec.Name, rt.executor),
		name:  rt.spec.Name, settings: settings, logFn: logStructuredWrapper})
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: sre actions not started: %v", rt.spec.Name, err)
		return
	}
	rt.sreActions = a
	rt.start(func() { a.Run(rt.ctx) })
	rt.note("sre_actions")
}

// dispatcherNotifier sends a Sage SRE approval request as the queue
// approval event of its item through the database's approval-card
// notifier (policy-08): chat channels get the card, bound to its channel,
// its content and backend incarnation, and a TTL of at most 15 minutes.
type dispatcherNotifier struct{ d executor.EventDispatcher }

func (n dispatcherNotifier) ApprovalRequested(ctx context.Context,
	r sreaction.ApprovalRequest) error {
	if n.d == nil {
		return fmt.Errorf("db %q: sre approval for queue item %d: no notifier", r.Database,
			r.QueueID)
	}
	if r.QueueID <= 0 {
		return fmt.Errorf("db %q: sre approval of proposal %s has no queue item", r.Database,
			r.ProposalID)
	}
	evt := notify.QueuedApprovalEvent(r.Title, r.SQL, r.Database, r.Risk, r.QueueID)
	evt.Body += "\n" + r.Summary
	return n.d.Dispatch(ctx, evt)
}
