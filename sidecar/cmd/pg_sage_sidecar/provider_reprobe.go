package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// The re-probe backoff of a runtime whose provider is unknown.
var (
	providerReprobeMin = time.Minute
	providerReprobeMax = 30 * time.Minute
)

// providerRebuildTimeout bounds building and publishing one replacement.
const providerRebuildTimeout = 2 * time.Minute

// providerReprobe re-probes an unknown provider with backoff until it gets a
// definite answer, then asks for this database's runtime to be rebuilt. The
// running runtime's provider is never changed in place. current is the
// provider the runtime runs with (a configured override while the probes
// were unknown); an answer equal to it needs no rebuild.
type providerReprobe struct {
	name     string
	current  string
	probe    func(context.Context) (string, error)
	rebuild  func(provider string) error
	sleep    func(context.Context, time.Duration) error
	min, max time.Duration
}

// run returns the provider a rebuild was requested for, or "" when ctx
// ended or the generation was replaced first.
func (r providerReprobe) run(ctx context.Context) string {
	wait := r.min
	for {
		if r.sleep(ctx, wait) != nil {
			return ""
		}
		wait = min(wait*2, r.max)
		provider, err := r.probe(ctx)
		if err != nil || provider == providerUnknown {
			logInfo("provider", "db %q: provider still unknown, next re-probe in %s: %v",
				r.name, wait, err)
			continue
		}
		if provider == r.current {
			logInfo("provider", "db %q: provider re-probe confirms the configured "+
				"provider %s", r.name, provider)
			return provider
		}
		logInfo("provider", "db %q: provider re-probe: %s -> %s; rebuilding the runtime",
			r.name, providerUnknown, provider)
		err = r.rebuild(provider)
		if err == nil {
			return provider
		}
		if errors.Is(err, fleet.ErrInstanceConflict) {
			logInfo("provider", "db %q: runtime was replaced; its successor probes "+
				"for itself", r.name)
			return ""
		}
		logWarn("provider", "db %q: rebuild for provider %s failed; provider stays "+
			"unknown and the re-probe retries: %v", r.name, provider, err)
	}
}

// detectProvider runs the startup detection, applies the configured
// provider override (which can only narrow) and logs the outcome. The probe
// outcome stays in rt.probed for the re-probe.
func (rt *databaseRuntime) detectProvider(ctx context.Context) string {
	probed, err := detectRuntimeProvider(ctx, rt.spec.Pool)
	rt.probed = probed
	provider, warning := resolveProviderOverride(probed, rt.spec.Config.Provider)
	if warning != "" {
		logWarn(rt.spec.Scope, "db %q: %s", rt.spec.Name, warning)
	}
	provider, warning = cnpgProvider(provider, rt.spec.Config.CNPG)
	if warning != "" {
		logWarn(rt.spec.Scope, "db %q: %s", rt.spec.Name, warning)
	}
	switch {
	case err != nil && provider != providerUnknown:
		logWarn(rt.spec.Scope, "db %q: provider probe failed after retries; using the "+
			"configured provider %s until a re-probe answers: %v", rt.spec.Name, provider, err)
	case err != nil:
		logWarn(rt.spec.Scope, "db %q: provider probe failed after retries; provider %s, "+
			"provider-gated actions wait for a successful re-probe: %v",
			rt.spec.Name, provider, err)
	default:
		logInfo(rt.spec.Scope, "db %q: cloud environment: %s", rt.spec.Name, provider)
	}
	return provider
}

// startProviderReprobe starts the re-probe of an unknown provider. It is not
// an instance worker: replacing this generation drains those, and the
// replacement is requested from here. It ends with this runtime's context.
func (rt *databaseRuntime) startProviderReprobe() {
	if !rt.needsProviderReprobe() || rt.spec.Pool == nil {
		return
	}
	detector := newProviderDetector(rt.spec.Pool)
	worker := providerReprobe{
		name: rt.spec.Name, current: rt.provider,
		probe: detector.probeOnce, rebuild: rt.rebuildForProvider,
		sleep: sleepContext, min: providerReprobeMin, max: providerReprobeMax,
	}
	go worker.run(rt.ctx)
}

// rebuildForProvider replaces this runtime generation with one built from
// scratch, which probes the provider itself. Standalone and the YAML control
// database have no single-runtime rebuild, so they report the finding as a
// readiness blocker instead.
func (rt *databaseRuntime) rebuildForProvider(provider string) error {
	if rt.spec.Shared || (rt.spec.Scope == "fleet" && rt.spec.ControlPool == rt.spec.Pool) {
		rt.markProviderPending(provider)
		return nil
	}
	parent := rt.spec.Parent
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, providerRebuildTimeout)
	defer cancel()
	candidate, err := rt.providerCandidate(ctx)
	if err != nil {
		return fmt.Errorf("db %q: prepare replacement runtime: %w", rt.spec.Name, err)
	}
	return replaceForProvider(ctx, fleetMgr, rt.inst, candidate)
}

func (rt *databaseRuntime) markProviderPending(provider string) {
	until := "provider-gated actions stay blocked until then"
	if rt.provider != providerUnknown {
		until = "the configured provider " + rt.provider + " applies until then"
	}
	logWarn(rt.spec.Scope, "db %q: provider re-probe found %s; restart pg_sage to apply "+
		"it (%s)", rt.spec.Name, provider, until)
	if rt.inst == nil {
		return
	}
	rt.inst.UpdateStatus(func(status *fleet.InstanceStatus) {
		status.Capabilities.PendingProvider = provider
	})
}

// providerCandidate builds the replacement through the mode's own runtime
// constructor, as a reload (YAML) or a reconnect (meta-db) does.
func (rt *databaseRuntime) providerCandidate(ctx context.Context) (*fleet.DatabaseInstance, error) {
	switch rt.spec.Scope {
	case "fleet":
		built, err := prepareFleetRuntime(currentFleetDatabase(rt.spec.Config), rt.spec.ControlPool)
		if err != nil {
			return nil, err
		}
		built.inst.DatabaseID = rt.inst.DatabaseID
		return built.inst, nil
	case "meta-db":
		state := globalMetaState
		if state == nil || state.Store == nil {
			return nil, errors.New("the meta-db store is unavailable")
		}
		rec, err := state.Store.Get(ctx, rt.spec.DatabaseID)
		if err != nil {
			return nil, fmt.Errorf("read database %d: %w", rt.spec.DatabaseID, err)
		}
		if !rec.RunsRuntime() { // disabled, or observe-only (fleetpol-01d)
			return nil, fmt.Errorf("database %d is disabled or observe-only: %w", rec.ID,
				fleet.ErrInstanceConflict)
		}
		return prepareStoreDatabase(ctx, state, *rec)
	}
	return nil, fmt.Errorf("no single-runtime rebuild for scope %q", rt.spec.Scope)
}

// currentFleetDatabase is the YAML entry as the latest reload left it.
func currentFleetDatabase(built config.DatabaseConfig) config.DatabaseConfig {
	if cfg == nil {
		return built
	}
	config.RLockForHotReload()
	defer config.RUnlockForHotReload()
	for _, db := range cfg.Databases {
		if db.Name == built.Name {
			return db
		}
	}
	return built
}

// replaceForProvider publishes candidate only while old is still the active
// generation, then drains old. A drain error after publication is a warning.
func replaceForProvider(
	ctx context.Context, mgr *fleet.DatabaseManager, old, candidate *fleet.DatabaseInstance,
) error {
	if mgr == nil || old == nil {
		return errors.Join(errors.New("no fleet registration to replace"),
			fleet.ShutdownInstance(context.WithoutCancel(ctx), candidate))
	}
	err := mgr.ReplaceInstanceIfCurrent(ctx, old.Name, old, candidate, healthCheckStoreDatabase)
	if err != nil && mgr.GetInstance(candidate.Name) != candidate {
		return err
	}
	if err != nil {
		logWarn("provider", "db %q: rebuilt, but the old runtime's drain is incomplete: %v",
			old.Name, err)
	}
	updateInstanceFindings(context.Background(), candidate)
	return nil
}
