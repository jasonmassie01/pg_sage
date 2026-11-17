package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

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

// verifierCache keeps the first verifier newBenchVerifier built without an
// error. Every database's ingester asks for one on every tick, and each
// build parses the embedded trusted root, so a fleet builds it once per
// process; a failed build is not kept and is retried on the next ask.
type verifierCache struct {
	mu sync.Mutex
	v  benchingest.Verifier
}

// benchVerifiers is the process's verifier cache.
var benchVerifiers verifierCache

// get is the cached verifier, built with build when there is none yet.
func (c *verifierCache) get(build func() (benchingest.Verifier,
	error)) (benchingest.Verifier, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.v != nil {
		return c.v, nil
	}
	v, err := build()
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, errors.New("the release verifier is missing")
	}
	c.v = v
	return v, nil
}

// reset forgets the cached verifier (tests swap newBenchVerifier).
func (c *verifierCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.v = nil
}

// benchVerifier is the process's release verifier.
func benchVerifier() (benchingest.Verifier, error) {
	return benchVerifiers.get(newBenchVerifier)
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

// benchIngester ingests every source now and on every tick, then records
// and tells any model-root authority change (a newer report, or the
// deciding one aging out).
func benchIngester(svc *earned.Service, s config.SREAutonomyConfig) func(context.Context) {
	sources := benchSources(s)
	return func(ctx context.Context) {
		res, err := ingestBenchSources(ctx, svc, sources)
		if err != nil {
			logWarn("autonomy", "ingest bench reports: %v", err)
		}
		info := func(format string, args ...any) { logInfo("autonomy", format, args...) }
		benchIngestNotices.note(res.Skipped, info)
		benchIngestNotices.noteRefused(res.Refused, info)
		if res.Added > 0 {
			logInfo("autonomy", "ingested %d new PGIncidentBench reports (%d signed) for %s",
				res.Added, res.Signed, svc.Build())
		}
		if _, err := svc.ReconcileRootAuthority(ctx); err != nil {
			logWarn("autonomy", "model-root authority: %v", err)
		}
	}
}

// benchNotices remembers the ingest notices already logged, keyed, so
// the hourly ingest of every database tells each one once per process.
type benchNotices struct {
	mu   sync.Mutex
	told map[string]bool
}

// benchIngestNotices is shared by every database's ledger.
var benchIngestNotices benchNotices

// first reports whether key is new, and remembers it.
func (n *benchNotices) first(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.told[key] {
		return false
	}
	if n.told == nil {
		n.told = map[string]bool{}
	}
	n.told[key] = true
	return true
}

// note logs each skipped replay report not logged before.
func (n *benchNotices) note(skipped []string, logf func(string, ...any)) {
	for _, path := range skipped {
		if n.first("replay:" + path) {
			logf("skipping %s: a PGIncidentBench replay-corpus report, not a bench report",
				path)
		}
	}
}

// noteRefused logs each refused report version (path and content hash)
// not logged before: a rewritten file is told again.
func (n *benchNotices) noteRefused(refused []benchingest.Refused,
	logf func(string, ...any)) {
	for _, r := range refused {
		if n.first("refused:" + r.Path + "@" + r.SHA256) {
			logf("bench report %s (sha256 %.12s) does not count: %s; it is logged once "+
				"until the file changes", r.Path, r.SHA256, r.Reason)
		}
	}
}

// ingestBenchSources ingests every source; without verification material
// signed reports go in unsigned.
func ingestBenchSources(ctx context.Context, svc *earned.Service,
	sources []benchingest.Source) (benchingest.Result, error) {
	verifier, err := benchVerifier()
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
		total.Skipped = append(total.Skipped, res.Skipped...)
		total.Refused = append(total.Refused, res.Refused...)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return total, errors.Join(errs...)
}

// localBenchProvider is the disposable target of a local bench run: the
// configured clone provider, else the local development database (game
// days need not be on), else none. guard refuses the monitored and the
// control databases.
func localBenchProvider(s config.SREAutonomyConfig, cloneCfg config.CloneProviderConfig,
	guard localTargetGuard) (clone.Provider, string, error) {
	if cloneCfg.Provider == "dle" || cloneCfg.Provider == clone.LakebaseAdapter ||
		cloneCfg.Provider == clone.NeonAdapter || cloneCfg.Provider == clone.AuroraAdapter {
		p, err := cloneProviderFor(cloneCfg, guard.own)
		return p, cloneCfg.Provider, err
	}
	if s.GameDays.LocalDSN == "" {
		return nil, "", nil
	}
	p, err := gameday.NewLocalProvider(s.GameDays.LocalDSN, guard.dsns, guard.pools)
	if err != nil {
		return nil, "", err
	}
	return p, "local", nil
}

// newLocalBenchFor is database's local bench (nil without a target). A
// run repeats until every family it covers reaches the promotion bar's
// sample size, and its report is stamped with this build.
func newLocalBenchFor(database string, svc *earned.Service, s config.SREAutonomyConfig,
	cloneCfg config.CloneProviderConfig, guard localTargetGuard) (*gameday.LocalBench,
	error) {
	provider, name, err := localBenchProvider(s, cloneCfg, guard)
	if err != nil || provider == nil {
		return nil, err
	}
	th := promotionThresholds(s.Promotion).Normalized()
	b := runningBuild()
	faults := srebench.GameDayFaults{MinRunsPerFamily: max(th.MinTop1N, th.MinSafePassN),
		PgSageVersion: b.Version, PgSageCommit: b.Commit}
	store, err := gameday.NewStore(svc.Store().Pool(), svc.Store().DeploymentID())
	if err != nil {
		return nil, fmt.Errorf("local bench run store: %w", err)
	}
	return gameday.NewLocalBench(gameday.LocalBenchConfig{Database: database, Provider: name,
		Store: store,
		Log:   func(format string, args ...any) { logWarn("autonomy", format, args...) },
		Warn:  func(format string, args ...any) { logWarn("autonomy", format, args...) }},
		provider, faults, svc)
}

// startLocalBench registers the database's local bench for the API.
func (rt *databaseRuntime) startLocalBench(svc *earned.Service, s config.SREAutonomyConfig) {
	bench, err := newLocalBenchFor(rt.spec.Name, svc, s, rt.cfg.Clone,
		localTargetGuardFor(rt.spec.Pool, svc.Store().Pool()))
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
		if n, err := bench.AbandonStale(rt.ctx); err != nil {
			logWarn("autonomy", "db %q: abandon stale local bench runs: %v", name, err)
		} else if n > 0 {
			logInfo("autonomy", "db %q: abandoned %d stale local bench run(s)", name, n)
		}
		<-rt.ctx.Done()
		benches.Remove(name)
	})
}

// verifyReportFile verifies a report's <report>.sigstore.json bundle and,
// when want is set, that it is the report of that commit.
func verifyReportFile(path, want string) (benchsig.Signature, error) {
	sig, _, err := readVerifiedReport(path, want)
	return sig, err
}

// verifiedReport is a report and its bundle, as read and verified.
type verifiedReport struct {
	report, bundle []byte
}

// readVerifiedReport reads a report and its bundle once and verifies
// those bytes, so a caller copying them copies exactly what verified.
func readVerifiedReport(path, want string) (benchsig.Signature, verifiedReport, error) {
	verifier, err := benchVerifier()
	if err != nil {
		return benchsig.Signature{}, verifiedReport{},
			fmt.Errorf("load the embedded Sigstore trusted root: %w", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return benchsig.Signature{}, verifiedReport{}, fmt.Errorf("read report: %w", err)
	}
	bundle, err := os.ReadFile(path + benchingest.BundleSuffix)
	if err != nil {
		return benchsig.Signature{}, verifiedReport{}, fmt.Errorf(
			"no signature bundle %s: %w", path+benchingest.BundleSuffix, err)
	}
	sig, err := verifier.Verify(raw, bundle)
	if err != nil {
		return benchsig.Signature{}, verifiedReport{}, err
	}
	if err := checkReportCommit(raw, sig, want); err != nil {
		return benchsig.Signature{}, verifiedReport{}, err
	}
	return sig, verifiedReport{report: raw, bundle: bundle}, nil
}
