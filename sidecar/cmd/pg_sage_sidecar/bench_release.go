package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/pg-sage/sidecar/internal/benchingest"
	"github.com/pg-sage/sidecar/internal/benchsig"
	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/gameday"
	srebench "github.com/pg-sage/sidecar/sre-bench"
)

// Roadmap 1.1 (2026-10-03) wiring of bench evidence: the reports shipped
// with this pg_sage (signed by the release workflow) and the operator's
// bench_results_path are ingested at startup and hourly; "Run bench
// locally" runs on a clone or the local development database.

// shippedBenchDirs are the directories shipped reports may be in.
var shippedBenchDirs = func() []string {
	exe, err := os.Executable()
	if err != nil {
		logWarn("autonomy", "locate the pg_sage binary for its bench reports: %v", err)
		exe = ""
	}
	return benchingest.ShippedDirs(exe)
}

// newBenchVerifier verifies release signatures against the embedded
// Sigstore trusted root.
var newBenchVerifier = func() (benchingest.Verifier, error) {
	v, err := benchsig.NewReleaseVerifier()
	if err != nil {
		return nil, err
	}
	return v, nil
}

// runningBuild is this binary's build (ldflags), normalized.
func runningBuild() earned.Build {
	return earned.Build{Version: version, Commit: commit}.Normalized()
}

// benchSources are the shipped directories, then bench_results_path.
func benchSources(s config.SREAutonomyConfig) []benchingest.Source {
	var out []benchingest.Source
	for _, dir := range shippedBenchDirs() {
		out = append(out, benchingest.Source{Path: dir, Shipped: true, Actor: "release_bench"})
	}
	if s.BenchResultsPath != "" {
		out = append(out, benchingest.Source{Path: s.BenchResultsPath,
			Actor: "bench_results_path"})
	}
	return out
}

// benchIngester ingests every source now and on every tick.
func benchIngester(svc *earned.Service, s config.SREAutonomyConfig) func(context.Context) {
	sources := benchSources(s)
	return func(ctx context.Context) {
		res, err := ingestBenchSources(ctx, svc, sources)
		if err != nil {
			logWarn("autonomy", "ingest bench reports: %v", err)
		}
		if res.Added > 0 {
			logInfo("autonomy", "ingested %d new PGIncidentBench reports (%d signed) for %s",
				res.Added, res.Signed, svc.Build())
		}
	}
}

// ingestBenchSources ingests every source; without verification material
// signed reports go in unsigned.
func ingestBenchSources(ctx context.Context, svc *earned.Service,
	sources []benchingest.Source) (benchingest.Result, error) {
	verifier, err := newBenchVerifier()
	if err != nil {
		logWarn("autonomy", "bench report signatures cannot be verified (%v); signed "+
			"reports are ingested as unsigned", err)
		verifier = nil
	}
	var total benchingest.Result
	var errs []error
	for _, src := range sources {
		res, err := benchingest.Ingest(ctx, svc, verifier, src)
		total.Files, total.Added = total.Files+res.Files, total.Added+res.Added
		total.Signed += res.Signed
		if err != nil {
			errs = append(errs, err)
		}
	}
	return total, errors.Join(errs...)
}

// localBenchProvider is the disposable target of a local bench run: the
// configured clone provider, else the local development database (game
// days need not be on), else none. refused are the monitored and the
// control databases' DSNs.
func localBenchProvider(s config.SREAutonomyConfig, cloneCfg config.CloneProviderConfig,
	refused []string) (clone.Provider, string, error) {
	if cloneCfg.Provider == "dle" || cloneCfg.Provider == "snapshot" {
		p, err := configuredMCPCloneProvider(cloneCfg)
		return p, cloneCfg.Provider, err
	}
	if s.GameDays.LocalDSN == "" {
		return nil, "", nil
	}
	p, err := gameday.NewLocalProvider(s.GameDays.LocalDSN, refused)
	if err != nil {
		return nil, "", err
	}
	return p, "local", nil
}

// newLocalBenchFor is database's local bench (nil without a target). A
// run repeats until every family it covers reaches the promotion bar's
// sample size, and its report is stamped with this build.
func newLocalBenchFor(database string, svc *earned.Service, s config.SREAutonomyConfig,
	cloneCfg config.CloneProviderConfig, refused []string) (*gameday.LocalBench, error) {
	provider, name, err := localBenchProvider(s, cloneCfg, refused)
	if err != nil || provider == nil {
		return nil, err
	}
	th := promotionThresholds(s.Promotion).Normalized()
	b := runningBuild()
	faults := srebench.GameDayFaults{MinRunsPerFamily: max(th.MinTop1N, th.MinSafePassN),
		PgSageVersion: b.Version, PgSageCommit: b.Commit}
	return gameday.NewLocalBench(gameday.LocalBenchConfig{Database: database, Provider: name,
		Log: func(format string, args ...any) { logWarn("autonomy", format, args...) }},
		provider, faults, svc)
}

// startLocalBench registers the database's local bench for the API.
func (rt *databaseRuntime) startLocalBench(svc *earned.Service, s config.SREAutonomyConfig) {
	refused := append(monitoredDSNs(rt.spec.Pool), svc.Store().Pool().Config().ConnString())
	bench, err := newLocalBenchFor(rt.spec.Name, svc, s, rt.cfg.Clone, refused)
	if err != nil {
		logError("autonomy", "db %q: local bench runs off: %v", rt.spec.Name, err)
		return
	}
	if bench == nil {
		return
	}
	benches := processAutonomy().localBenches
	benches.Register(rt.spec.Name, bench)
	name := rt.spec.Name
	rt.start(func() {
		<-rt.ctx.Done()
		benches.Remove(name)
	})
}

// verifyReportFile verifies a report's <report>.sigstore.json bundle and,
// when want is set, that it is the report of that commit.
func verifyReportFile(path, want string) (benchsig.Signature, error) {
	verifier, err := newBenchVerifier()
	if err != nil {
		return benchsig.Signature{}, fmt.Errorf("load the embedded Sigstore trusted root: %w",
			err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return benchsig.Signature{}, fmt.Errorf("read report: %w", err)
	}
	bundle, err := os.ReadFile(path + benchingest.BundleSuffix)
	if err != nil {
		return benchsig.Signature{}, fmt.Errorf("no signature bundle %s: %w",
			path+benchingest.BundleSuffix, err)
	}
	sig, err := verifier.Verify(raw, bundle)
	if err != nil {
		return benchsig.Signature{}, err
	}
	return sig, checkReportCommit(raw, sig, want)
}
