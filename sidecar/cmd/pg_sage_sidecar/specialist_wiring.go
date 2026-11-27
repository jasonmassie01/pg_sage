package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/specialist"
)

// The Postgres-specialist contract (roadmap phase 3) for the sidecar: one
// service shared by the HTTP handler, the adapters and the MCP tools (one
// set of per-identity limits), the outbound result worker, and the
// custodian re-scan that turns a requested custodian action into an
// ordinary proposal.

// specialistRuntime is the process's contract wiring.
type specialistRuntime struct {
	svc     *specialist.Service
	handler http.Handler
	mcp     *mcp.SpecialistService
	worker  *specialist.OutboundWorker
	store   specialist.RequestStore
}

// specialistOutbound timings.
const (
	specialistOutboundInterval = 15 * time.Second
	specialistOutboundAttempts = 5
	specialistOutboundGiveUp   = 2 * time.Hour
)

var (
	specialistProcess   atomic.Pointer[specialistRuntime]
	specialistCustodian = newSpecialistCustodians()
)

// buildSpecialist wires the contract on the control pool; nil without one
// or when the contract is disabled.
func buildSpecialist(cfg *config.Config, mgr *fleet.DatabaseManager, pool *pgxpool.Pool,
	registry *earned.Registry, custodians *specialistCustodians) (*specialistRuntime,
	error) {
	if pool == nil {
		return nil, nil
	}
	return buildSpecialistWithStore(cfg, mgr, specialist.NewPGStore(pool),
		specialist.NewTokenAuthenticator(mcptoken.NewStore(pool)), custodians,
		registry)
}

// specialistLimits are the contract's bounds from configuration.
func specialistLimits(sc config.SpecialistConfig) specialist.Limits {
	return specialist.Limits{WritesPerMinute: sc.WritesPerMinute,
		ReadsPerMinute: sc.ReadsPerMinute, MaxOpenPerIdentity: sc.MaxOpenPerIdentity,
		MaxOpenTotal: sc.MaxOpenTotal, MaxLiveAge: sc.MaxLiveAge()}
}

// buildSpecialistWithStore wires the contract on store (tests pass a
// memory store and no authenticator).
func buildSpecialistWithStore(cfg *config.Config, mgr *fleet.DatabaseManager,
	store specialist.RequestStore, auth specialist.Authenticator,
	custodians *specialistCustodians, registry ...*earned.Registry) (*specialistRuntime,
	error) {
	sc := cfg.Specialist
	if !sc.Enabled {
		return nil, nil
	}
	deps := specialist.Deps{Directory: specialist.NewFleetDirectory(mgr, custodians),
		Store: store, KeepIdentifiers: sc.KeepIdentifiers, Logger: slog.Default(),
		Limits: specialistLimits(sc)}
	if len(registry) > 0 && registry[0] != nil {
		deps.Calibrator = specialist.BenchCalibrator{Registry: registry[0]}
	}
	svc, err := specialist.NewService(deps)
	if err != nil {
		return nil, fmt.Errorf("specialist contract: %w", err)
	}
	opts, err := specialistHandlerOptions(sc)
	if err != nil {
		return nil, err
	}
	if auth == nil {
		auth = specialist.NewTokenAuthenticator(nil)
	}
	return &specialistRuntime{svc: svc, store: store,
		handler: specialist.NewHandler(svc, auth, opts),
		mcp:     &mcp.SpecialistService{Service: svc},
		worker:  specialistWorker(sc, svc, store)}, nil
}

func specialistHandlerOptions(sc config.SpecialistConfig) (specialist.HandlerOptions,
	error) {
	var opts specialist.HandlerOptions
	if sc.PagerDuty.SigningSecret != "" || len(sc.PagerDuty.Services) > 0 {
		routes, err := specialist.ParsePagerDutyServices(sc.PagerDuty.Services)
		if err != nil {
			return opts, fmt.Errorf("specialist.pagerduty.services: %w", err)
		}
		opts.PagerDuty = &specialist.PagerDutyAdapter{Secret: sc.PagerDuty.SigningSecret,
			Services: routes}
	}
	if sc.Webhook.SigningSecret != "" {
		opts.Webhook = &specialist.WebhookAdapter{Secret: sc.Webhook.SigningSecret,
			Tolerance: sc.Webhook.Tolerance()}
	}
	return withAlertAdapters(opts, sc)
}

// withAlertAdapters adds the incident.io and Azure Monitor adapters (mcp-02),
// each off until its secret is configured.
func withAlertAdapters(opts specialist.HandlerOptions,
	sc config.SpecialistConfig) (specialist.HandlerOptions, error) {
	if io := sc.IncidentIO; io.SigningSecret != "" {
		routes, err := specialist.ParseAlertRoutes("specialist.incidentio.routes",
			io.Routes, true)
		if err != nil {
			return opts, err
		}
		opts.IncidentIO = &specialist.IncidentIOAdapter{Secret: io.SigningSecret,
			Field: io.DatabaseField, Routes: routes}
	}
	if az := sc.AzureMonitor; az.SharedSecret != "" {
		routes, err := specialist.ParseAlertRoutes("specialist.azure_monitor.resources",
			az.Resources, true)
		if err != nil {
			return opts, err
		}
		opts.AzureMonitor = &specialist.AzureMonitorAdapter{Secret: az.SharedSecret,
			Resources: routes}
	}
	return opts, nil
}

// specialistWorker posts results only to the endpoints the operator
// configured.
func specialistWorker(sc config.SpecialistConfig, svc *specialist.Service,
	store specialist.RequestStore) *specialist.OutboundWorker {
	notifiers := map[string]specialist.Notifier{}
	if sc.PagerDuty.APIURL != "" {
		notifiers["pagerduty"] = &specialist.PagerDutyNotifier{BaseURL: sc.PagerDuty.APIURL,
			Token: sc.PagerDuty.APIToken, From: sc.PagerDuty.FromEmail}
	}
	if sc.Webhook.ResultURL != "" {
		notifiers["webhook"] = &specialist.WebhookNotifier{URL: sc.Webhook.ResultURL,
			Secret: sc.Webhook.SigningSecret}
	}
	return &specialist.OutboundWorker{Store: store, Service: svc, Notifiers: notifiers,
		Now: time.Now, Interval: specialistOutboundInterval,
		MaxAttempts: specialistOutboundAttempts, GiveUpAfter: specialistOutboundGiveUp}
}

// specialistAPIDeps builds the process's contract for the API router: the
// handler and the request audit, both nil when there is none.
func specialistAPIDeps(c *config.Config, mgr *fleet.DatabaseManager,
	pool *pgxpool.Pool) (http.Handler, specialistAuditReader) {
	if c == nil {
		return nil, nil
	}
	rt, err := buildSpecialist(c, mgr, pool, processAutonomy().registry, specialistCustodian)
	if err != nil {
		logError("specialist", "contract not served: %v", err)
		return nil, nil
	}
	specialistProcess.Store(rt)
	if rt == nil {
		return nil, nil
	}
	return rt.handler, rt.store
}

type specialistAuditReader = specialist.RequestStore

// startSpecialistOutbound runs the result worker until ctx ends.
func startSpecialistOutbound(ctx context.Context) {
	rt := specialistProcess.Load()
	if rt == nil || rt.worker == nil || len(rt.worker.Notifiers) == 0 {
		return
	}
	go rt.worker.Run(ctx)
}

// processSpecialistMCP resolves the process contract at call time (the MCP
// runtime starts before the API router builds it).
type processSpecialistMCP struct{}

func (processSpecialistMCP) SpecialistCall(ctx context.Context, tool string,
	caller mcp.SpecialistCaller, database string, args json.RawMessage) (any, error) {
	rt := specialistProcess.Load()
	if rt == nil {
		return nil, fmt.Errorf("%w: the specialist contract is not enabled",
			specialist.ErrUnavailable)
	}
	return rt.mcp.SpecialistCall(ctx, tool, caller, database, args)
}

// specialistCustodians maps databases to their runway advisors, which
// re-scan custodians for a requested custodian action.
type specialistCustodians struct {
	mu       sync.RWMutex
	advisors map[string]*runwayAdvisor
}

func newSpecialistCustodians() *specialistCustodians {
	return &specialistCustodians{advisors: map[string]*runwayAdvisor{}}
}

func (c *specialistCustodians) register(database string, a *runwayAdvisor) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a == nil {
		delete(c.advisors, database)
		return
	}
	c.advisors[database] = a
}

func (c *specialistCustodians) remove(database string) { c.register(database, nil) }

func (c *specialistCustodians) advisor(database string) *runwayAdvisor {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.advisors[database]
}
