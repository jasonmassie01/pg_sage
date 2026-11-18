package main

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/drill"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Agent governance gate composition (spec §6.2.1): policy defines
// AgentDecider, cmd injects decide.Decider into every database's standing
// gate and the §6.2.7 hold into every executor. Executors are built before
// the control database is known, so both read the process's governance at
// decision time: until it starts (or without a control database) agent
// requests are capped at approval and agent changes fail closed.

type agentGateState struct {
	decider *decide.Decider
	control *pgxpool.Pool
	// envs classifies databases (nil without environment bindings); the
	// drill scheduler reads it to find production databases.
	envs *envbind.Service
}

var agentGate atomic.Pointer[agentGateState]

// startAgentGate builds the decider on the governance control database.
// Without one governance is posture-only and the gate keeps its L2 cap.
func startAgentGate(c *config.Config, mgr *fleet.DatabaseManager, meta *metaDBState,
	envs *envbind.Service) {
	control := governanceControlPool(c, mgr, meta)
	if control == nil {
		return
	}
	cfg := decide.Config{Principals: agentguard.NewStore(control),
		Profiles: decide.DefaultProfiles(),
		Recovery: fleetRecovery{mgr: mgr, drills: drill.NewStore(control)},
		Freezes:  agentFreezes(control)}
	agentGrantSources(&cfg, mgr) // D5, D9 and D10 (agent_grants_wiring.go)
	if envs != nil {
		cfg.Environments = envSource{svc: envs}
		// D5 for brokered requests: the catalog and classification check
		// (agent_broker_wiring.go) and the grant registry's, both must pass.
		cfg.Objects = allObjects{agentObjectChecker(envs), cfg.Objects}
	}
	agentGate.Store(&agentGateState{decider: decide.New(cfg), control: control, envs: envs})
}

// installAgentGovernance wires one database's executor.
func installAgentGovernance(ex *executor.Executor, database string) {
	ex.WithAgentDecider(lateDecider{database: database})
	ex.WithPrincipalHold(holdOnControl)
}

// lateDecider reads the process's decider at decision time.
type lateDecider struct{ database string }

func (l lateDecider) Decide(ctx context.Context, req policy.ActionRequest) (
	policy.Decision, int, bool) {
	state := agentGate.Load()
	if state == nil {
		return policy.Decision{}, policy.AgentUngovernedCap, false
	}
	return state.decider.Policy(l.database).Decide(ctx, req)
}

// holdOnControl is the §6.2.7 hold on the governance control database.
func holdOnControl(ctx context.Context, principalID string) (func(), error) {
	state := agentGate.Load()
	if state == nil {
		return nil, agentguard.ErrUnavailable
	}
	return decide.HoldActive(ctx, state.control, principalID)
}

// envSource evaluates a fleet database's environment from its binding.
type envSource struct{ svc *envbind.Service }

func (e envSource) Environment(ctx context.Context, database string) (envbind.Binding,
	error) {
	db, err := e.svc.Resolve(ctx, database)
	if err != nil {
		return envbind.Binding{}, err
	}
	return e.svc.Binder.EnvironmentOf(ctx, db)
}

// bindStdioPrincipal binds the stdio client to mcp.stdio_principal. A name
// that does not resolve leaves stdio unbound (read and propose, capped at
// approval; agent_* tools agent_unsponsored) and says why once.
func bindStdioPrincipal(rt *mcp.Runtime, c *config.Config, mgr *fleet.DatabaseManager,
	meta *metaDBState) {
	if rt == nil || c == nil || c.MCP.StdioPrincipal == "" {
		return
	}
	control := governanceControlPool(c, mgr, meta)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := agentguard.NewStore(control).GetByName(ctx, c.MCP.StdioPrincipal)
	if err != nil {
		logError("mcp", "mcp.stdio_principal %q: %v: stdio stays unbound (read and "+
			"propose tools, capped at approval)", c.MCP.StdioPrincipal, err)
		return
	}
	rt.SetStdioPrincipal(p.ID)
}

// fleetRecovery is D7's recovery posture: the provider's PITR flag from
// cloud telemetry, else the server's WAL archiving, and the database's
// last passed restore drill (drill-01a; zero, which caps L3 at L2, when
// none passed or the drill record cannot be read).
type fleetRecovery struct {
	mgr    *fleet.DatabaseManager
	drills drillHistory
	// cloudPITR is the provider's PITR flag (nil: unknown); nil is cloud
	// telemetry's.
	cloudPITR func(database string) *bool
}

// drillHistory is where passed restore drills are read (drill.Store).
type drillHistory interface {
	LastPassed(ctx context.Context, database string) (time.Time, error)
}

// drillReadWarned keeps one warning per database for an unreadable drill
// record.
var drillReadWarned sync.Map

func (f fleetRecovery) Recovery(ctx context.Context, database string) (bool, time.Time,
	error) {
	lookup := f.cloudPITR
	if lookup == nil {
		lookup = cloudTelemetryPITR
	}
	if pitr := lookup(database); pitr != nil {
		return *pitr, f.lastDrill(ctx, database), nil
	}
	var inst *fleet.DatabaseInstance
	if f.mgr != nil {
		inst = f.mgr.GetInstance(database)
	}
	if inst == nil || inst.Pool == nil {
		return false, time.Time{}, nil
	}
	pitr, _, err := walArchived(ctx, inst.Pool)
	if err != nil {
		return false, time.Time{}, err
	}
	return pitr, f.lastDrill(ctx, database), nil
}

// lastDrill is the database's last passed drill, zero when none passed.
// A read failure is zero too: D7 then caps at L2 (fail closed) instead of
// refusing every production write.
func (f fleetRecovery) lastDrill(ctx context.Context, database string) time.Time {
	if f.drills == nil {
		return time.Time{}
	}
	at, err := f.drills.LastPassed(ctx, database)
	if err != nil {
		if _, seen := drillReadWarned.LoadOrStore(database, true); !seen {
			logWarn("agents", "D7 reads no restore drill for %s, so agent writes stay at "+
				"L2: %v", database, err)
		}
		return time.Time{}
	}
	drillReadWarned.Delete(database)
	return at
}

// cloudTelemetryPITR is the provider's PITR flag from cloud telemetry.
func cloudTelemetryPITR(database string) *bool {
	if rt := cloudtel.Lookup(database); rt != nil {
		if b := rt.Backup(); b != nil {
			return b.PITREnabled
		}
	}
	return nil
}

func walArchived(ctx context.Context, pool *pgxpool.Pool) (bool, time.Time, error) {
	var mode, command, library string
	err := pool.QueryRow(ctx, `/* pg_sage agent_recovery_posture v1 */
SELECT current_setting('archive_mode'), COALESCE(current_setting('archive_command', true), ''),
       COALESCE(current_setting('archive_library', true), '')`).
		Scan(&mode, &command, &library)
	if err != nil {
		return false, time.Time{}, err
	}
	archives := strings.TrimSpace(library) != "" ||
		(strings.TrimSpace(command) != "" && command != "(disabled)")
	return (mode == "on" || mode == "always") && archives, time.Time{}, nil
}

// allObjects is D5 from several sources: the first denial wins.
type allObjects []decide.ObjectChecker

func (a allObjects) CheckObjects(ctx context.Context, p agentguard.Principal,
	database string, env envbind.Env, objects []decide.Object) error {
	for _, c := range a {
		if c == nil {
			continue
		}
		if err := c.CheckObjects(ctx, p, database, env, objects); err != nil {
			return err
		}
	}
	return nil
}
