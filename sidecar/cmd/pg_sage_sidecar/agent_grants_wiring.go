package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/agentguard/grants"
	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/store"
)

// Agent grants (spec §6.3, §6.6): the grant service behind the REST
// routes and agent_request_capability, the grant side of the gate's D5,
// D9 and D10, and the expiry reconciler on the leader. A grant's target
// is resolved at call time: its pool, executor, SRE binding id and the
// environment envbind evaluates now (never a claimed one).

// agentGrantPass bounds the grants one reconcile pass revokes per database.
const agentGrantPass = 200

var (
	agentGrantMu  sync.Mutex
	agentGrantSvc *grants.Service
)

// agentGrantService builds the process's grant service the first time a
// control database is connected; nil until then (agent governance is
// posture-only).
func agentGrantService(c *config.Config, mgr *fleet.DatabaseManager,
	meta *metaDBState) *grants.Service {
	agentGrantMu.Lock()
	defer agentGrantMu.Unlock()
	if agentGrantSvc != nil || c == nil {
		return agentGrantSvc
	}
	control := governanceControlPool(c, mgr, meta)
	if control == nil {
		return nil
	}
	m, err := grants.NewManager(agentguard.NewStore(control), grants.Config{
		MaxDuration: c.Agents.Capabilities.MaxDuration()})
	if err != nil {
		logError("agents", "agent grants unavailable: %v", err)
		return nil
	}
	agentGrantSvc = &grants.Service{Manager: m, Principals: agentguard.NewStore(control),
		Resolve: agentGrantTarget(mgr), Decider: lateGrantDecider{},
		RequestTTL: grants.DefaultRequestTTL}
	return agentGrantSvc
}

// lateGrants resolves the grant service at call time, so routes and tools
// wired before the fleet connects still reach it.
type lateGrants struct {
	c    *config.Config
	mgr  *fleet.DatabaseManager
	meta *metaDBState
}

var errGrantsUnavailable = fmt.Errorf("%w: agent grants need mode: meta or "+
	"agents.control_database", agentguard.ErrUnavailable)

func (l lateGrants) svc() (*grants.Service, error) {
	if s := agentGrantService(l.c, l.mgr, l.meta); s != nil {
		return s, nil
	}
	return nil, errGrantsUnavailable
}

// agentGrantAPI is the router's grant service.
func agentGrantAPI(c *config.Config, mgr *fleet.DatabaseManager,
	meta *metaDBState) api.AgentGrantService {
	return lateGrantAPI{lateGrants{c: c, mgr: mgr, meta: meta}}
}

// agentGrantMCP is agent_request_capability's backend.
func agentGrantMCP(c *config.Config, mgr *fleet.DatabaseManager,
	meta *metaDBState) mcp.GrantToolBackend {
	return mcpGrants{lateGrants{c: c, mgr: mgr, meta: meta}}
}

// agentGrantTarget resolves a fleet database for a grant.
func agentGrantTarget(mgr *fleet.DatabaseManager) func(context.Context, string) (
	grants.Target, error) {
	return func(ctx context.Context, name string) (grants.Target, error) {
		envs := agentEnvSvc
		if envs == nil || mgr == nil {
			return grants.Target{}, fmt.Errorf("%w: environments are not bound",
				agentguard.ErrUnavailable)
		}
		db, err := envs.Resolve(ctx, name)
		if err != nil {
			return grants.Target{}, err
		}
		inst := mgr.GetInstance(name)
		if inst == nil || inst.Executor == nil {
			return grants.Target{}, fmt.Errorf("%w: database %s has no executor",
				agentguard.ErrUnavailable, name)
		}
		// An unreadable identity evaluates as prod, with the error logged.
		b, err := envs.Binder.EnvironmentOf(ctx, db)
		if err != nil {
			logWarn("agents", "environment of %s unreadable, granting as prod: %v", name, err)
		}
		return grants.Target{Name: name, ID: db.ID, Pool: db.Pool, Env: b.Env,
			Executor: inst.Executor}, nil
	}
}

// lateGrantDecider reads the process's decider at decision time; before
// the gate starts every request is refused as unavailable.
type lateGrantDecider struct{}

func (lateGrantDecider) Decide(ctx context.Context, req decide.Request) decide.Verdict {
	state := agentGate.Load()
	if state == nil {
		return decide.Verdict{Reason: decide.ReasonUnavailable,
			Detail: "agent governance has not started"}
	}
	return state.decider.Decide(ctx, req)
}

// agentGrantSources are the grant side of the decider: D5 objects, D10
// leases and D9's pending count across the fleet.
func agentGrantSources(cfg *decide.Config, mgr *fleet.DatabaseManager) {
	checker := &grants.Checker{Resolve: agentGrantTarget(mgr)}
	cfg.Objects, cfg.Leases = checker, checker
	cfg.Pending = func(ctx context.Context, principalID string) (int, error) {
		return pendingForPrincipal(ctx, mgr, principalID)
	}
}

// pendingForPrincipal counts an agent's pending approvals in every
// database: queued actions and capability requests. A database that
// cannot be read fails the count (D9 then fails closed).
func pendingForPrincipal(ctx context.Context, mgr *fleet.DatabaseManager,
	principalID string) (int, error) {
	if mgr == nil || !agentguard.ValidID(principalID) {
		return 0, nil
	}
	total := 0
	for name, inst := range mgr.Instances() {
		if inst == nil || inst.Pool == nil {
			continue
		}
		queued, err := store.NewActionStore(inst.Pool).CountPendingForPrincipal(ctx,
			principalID)
		if err != nil {
			return 0, fmt.Errorf("pending approvals in %s: %w", name, err)
		}
		requests, err := grants.CountPendingRequests(ctx, inst.Pool, principalID)
		if err != nil {
			return 0, fmt.Errorf("pending requests in %s: %w", name, err)
		}
		total += queued + requests
	}
	return total, nil
}

// startAgentGrantReconciler revokes expired grants every
// agents.reconcile_interval_seconds on the leader, each revoke fenced by
// the lease in the governance control database.
func startAgentGrantReconciler(ctx context.Context, mgr *fleet.DatabaseManager,
	control *pgxpool.Pool, lead governanceLeader) {
	if cfg == nil {
		return
	}
	go every(ctx, cfg.Agents.ReconcileInterval(), func(c context.Context) {
		if svc := agentGrantService(cfg, mgr, globalMetaState); svc != nil {
			runAgentGrantExpiry(c, svc, mgr, control, lead)
		}
	})
}

func runAgentGrantExpiry(ctx context.Context, svc *grants.Service,
	mgr *fleet.DatabaseManager, control *pgxpool.Pool, lead governanceLeader) {
	f, ok := lead.fence("agent grant expiry")
	if !ok || mgr == nil {
		return
	}
	fence := grants.Fence{Control: control, Scope: f.Scope, Holder: f.Holder, Epoch: f.Epoch}
	for name := range mgr.Instances() {
		t, err := svc.Resolve(ctx, name)
		if errors.Is(err, envbind.ErrUnknownDatabase) || errors.Is(err,
			agentguard.ErrUnavailable) {
			continue
		}
		if err != nil {
			logWarn("agents", "grant expiry of %s: %v", name, err)
			continue
		}
		rep, err := svc.Manager.ExpireDue(ctx, t, fence, agentGrantPass)
		if errors.Is(err, grants.ErrFenced) {
			logWarn("agents", "grant expiry stopped: the leader lease moved mid-pass")
			return
		}
		logGrantExpiry(name, rep, err)
	}
}

func logGrantExpiry(name string, rep grants.ExpiryReport, err error) {
	if err != nil {
		logWarn("agents", "grant expiry of %s failed: %v", name, err)
	}
	if n := len(rep.Revoked); n > 0 {
		logInfo("agents", "%s: revoked %d expired agent grants", name, n)
	}
	for id, ferr := range rep.Failed {
		logWarn("agents", "%s: revoking expired grant %d failed, retried next pass: %v",
			name, id, ferr)
	}
	for _, id := range rep.Incomplete {
		logWarn("agents", "%s: grant %d expired but another grantor's privilege remains "+
			"(revoke_incomplete); the agent is denied that object until it is revoked",
			name, id)
	}
}

// mcpGrants adapts the grant service to agent_request_capability.
type mcpGrants struct{ late lateGrants }

func (m mcpGrants) RequestCapability(ctx context.Context, principalID string,
	in mcp.CapabilityRequest) (mcp.CapabilityResult, error) {
	svc, err := m.late.svc()
	if err != nil {
		return mcp.CapabilityResult{}, err
	}
	req := grants.CapabilityRequest{Database: in.Database, Capability: in.Capability,
		DurationMinutes: in.DurationMinutes, Reason: in.Reason}
	if id, ok := agentguard.IdentityFromContext(ctx); ok {
		req.TaskID = id.TaskID
	}
	for _, o := range in.Objects {
		req.Objects = append(req.Objects, grants.ObjectRequest{Object: o.Object,
			Columns: o.Columns})
	}
	res, err := svc.RequestCapability(ctx, principalID, req)
	return mcp.CapabilityResult{Verdict: res.Verdict, ReasonCode: res.ReasonCode,
		RequestID: res.RequestID, ExpiresAt: res.ExpiresAt, ApprovalURL: res.ApprovalURL,
		Detail: res.Detail, Fix: res.Fix, RetryAfterSeconds: res.RetryAfterSeconds}, err
}
