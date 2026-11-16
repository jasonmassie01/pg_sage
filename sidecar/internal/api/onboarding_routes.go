package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/firstlook"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/onboarding"
)

// registerOnboardingRoutes serves the first-run checklist, the first look
// and the trust guide to every signed-in role; which MCP tokens and
// notification channels exist stays visible to admins only. Operators and
// admins may re-check a database's first look. Admins record the setup
// wizard's steps (ux-04b); budgets is where the Model step reads the model.
func registerOnboardingRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager,
	cfg *config.Config, control *pgxpool.Pool, budgets LLMBudgetRegistry) {
	reader := newFleetOnboardingReader(mgr, cfg, control, budgets)
	route(mux, "GET /api/v1/onboarding", gateViewer, onboardingHandler(reader))
	route(mux, "POST /api/v1/onboarding/setup", gateAdmin,
		onboardingSetupHandler(reader, time.Now))
	route(mux, "GET /api/v1/onboarding/trust", gateViewer, trustGuideHandler(reader))
	route(mux, "GET /api/v1/first-look", gateViewer, firstLookHandler(reader))
	registerFirstLookRecheck(mux, fleetRecheckSource{mgr: mgr},
		newRecheckLimiter(firstLookRecheckEvery, time.Now))
}

// fleetOnboardingReader reads onboarding state from each database's
// runtime and its sage schema.
type fleetOnboardingReader struct {
	mgr     *fleet.DatabaseManager
	cfg     *config.Config
	control *pgxpool.Pool
	budgets LLMBudgetRegistry
}

func newFleetOnboardingReader(mgr *fleet.DatabaseManager, cfg *config.Config,
	control *pgxpool.Pool, budgets LLMBudgetRegistry) onboardingReader {
	if mgr == nil || cfg == nil {
		return nil
	}
	return &fleetOnboardingReader{mgr: mgr, cfg: cfg, control: control, budgets: budgets}
}

func (f *fleetOnboardingReader) Names(database string) ([]string, bool) {
	if database != "" && database != "all" {
		return []string{database}, f.mgr.GetInstance(database) != nil
	}
	instances := f.mgr.Instances()
	names := make([]string, 0, len(instances))
	for name := range instances {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, true
}

func (f *fleetOnboardingReader) instance(name string) (*fleet.DatabaseInstance, error) {
	inst := f.mgr.GetInstance(name)
	if inst == nil || inst.Pool == nil {
		return nil, fmt.Errorf("database %q: %w", name, errNoRuntime)
	}
	return inst, nil
}

// Database reads the status snapshot first, so a database whose reads
// fail is still named with its connection status and trust level.
func (f *fleetOnboardingReader) Database(ctx context.Context, name string) (
	onboardingDatabase, error) {
	db := onboardingDatabase{Name: name}
	inst := f.mgr.GetInstance(name)
	if inst == nil {
		return db, fmt.Errorf("database %q: %w", name, errNoRuntime)
	}
	status := inst.SnapshotStatus()
	db.Connected, db.TrustLevel = status.Connected, status.TrustLevel
	if inst.Executor != nil {
		db.TrustLevel = inst.Executor.TrustLevel()
	}
	if inst.Pool == nil {
		return db, fmt.Errorf("database %q: %w", name, errNoRuntime)
	}
	st, found, err := onboarding.Get(ctx, inst.Pool, name)
	if err != nil {
		return db, err
	}
	if found {
		db.State = &st
	}
	look, found, err := firstlook.NewStore(inst.Pool).LatestSummary(ctx, name)
	if err != nil {
		return db, err
	}
	if found {
		db.FirstLook = &look
	}
	return db, nil
}

func (f *fleetOnboardingReader) FirstLook(ctx context.Context, name string) (
	*firstlook.Report, error) {
	inst, err := f.instance(name)
	if err != nil {
		return nil, err
	}
	report, found, err := firstlook.NewStore(inst.Pool).Latest(ctx, name)
	if err != nil || !found {
		return nil, err
	}
	return &report, nil
}

func isAdmin(ctx context.Context) bool {
	u := UserFromContext(ctx)
	return u != nil && u.Role == "admin"
}

func (f *fleetOnboardingReader) MCP(ctx context.Context) (bool, *bool) {
	on := f.cfg.MCP.Enabled && f.cfg.MCP.Transport == "http"
	if !on || f.control == nil || !isAdmin(ctx) {
		return on, nil
	}
	return on, f.exists(ctx, "mcp tokens", `SELECT EXISTS (SELECT 1 FROM sage.mcp_tokens
		WHERE revoked_at IS NULL AND expires_at > now())`)
}

func (f *fleetOnboardingReader) Notifications(ctx context.Context) *bool {
	// Configured means alerting is on and a route reaches a usable channel,
	// not merely that a URL or key is set (alerting-08).
	if a := f.cfg.Alerting; a.Enabled && a.HasUsableRoute() {
		yes := true
		return &yes
	}
	if f.control == nil || !isAdmin(ctx) {
		return nil
	}
	return f.exists(ctx, "notification channels", `SELECT EXISTS (SELECT 1
		FROM sage.notification_channels WHERE enabled)`)
}

// exists runs a boolean control-database query; nil when it fails.
func (f *fleetOnboardingReader) exists(ctx context.Context, what, sql string) *bool {
	var ok bool
	if err := f.control.QueryRow(ctx, "/* pg_sage onboarding */ "+sql).Scan(&ok); err != nil {
		slog.Warn("onboarding: read "+what, "error", err)
		return nil
	}
	return &ok
}

func (f *fleetOnboardingReader) Guide(ctx context.Context, name string) (
	onboarding.GuideInput, grantTarget, error) {
	inst, err := f.instance(name)
	if err != nil {
		return onboarding.GuideInput{}, grantTarget{}, err
	}
	in := onboarding.GuideInput{Current: inst.SnapshotStatus().TrustLevel,
		SafeRampHours:     float64(f.cfg.Trust.RampSafeHours),
		ModerateRampHours: float64(f.cfg.Trust.RampModerateHours),
		Tier3Safe:         f.cfg.Trust.Tier3Safe, Tier3Moderate: f.cfg.Trust.Tier3Moderate}
	if inst.Executor != nil {
		in.Current, in.ExecutionMode = inst.Executor.TrustLevel(),
			inst.Executor.ExecutionMode()
	}
	in.RampElapsedHours, err = rampElapsedHours(ctx, inst.Pool)
	if err != nil {
		return onboarding.GuideInput{}, grantTarget{}, err
	}
	if in.Grants, err = onboarding.CheckGrants(ctx, inst.Pool); err != nil {
		return onboarding.GuideInput{}, grantTarget{}, err
	}
	if inst.ActionPool != nil { // the action identity's grants (security-09)
		action, err := onboarding.CheckGrants(ctx, inst.ActionPool)
		if err != nil {
			return onboarding.GuideInput{}, grantTarget{}, fmt.Errorf(
				"read the action identity's grants: %w", err)
		}
		in.ActionGrants = &action
	}
	return in, f.target(inst), nil
}

// target is how this mode changes trust.level: the config API (global, or
// the database's row in meta-db mode) or, for YAML fleets, the file.
func (f *fleetOnboardingReader) target(inst *fleet.DatabaseInstance) grantTarget {
	t := grantTarget{Method: "api", Key: "trust.level", ConfigURL: "/api/v1/config/global"}
	switch {
	case f.cfg.HasMetaDB() && inst.DatabaseID > 0:
		t.ConfigURL = fmt.Sprintf("/api/v1/config/databases/%d", inst.DatabaseID)
	case f.cfg.IsFleet():
		t = grantTarget{Method: "yaml", Key: "trust.level"}
	}
	return t
}

// rampElapsedHours reads how long the install's trust ramp has run; nil
// before the executor started it. PostgreSQL parses the stored timestamp,
// whichever format wrote it.
func rampElapsedHours(ctx context.Context, pool *pgxpool.Pool) (*float64, error) {
	var h float64
	err := pool.QueryRow(ctx, `/* pg_sage onboarding */ SELECT
		extract(epoch FROM now() - value::timestamptz)::float8 / 3600 FROM sage.config
		WHERE key = 'trust_ramp_start' AND value ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}'
		LIMIT 1`).Scan(&h)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read trust ramp start: %w", err)
	}
	return &h, nil
}
