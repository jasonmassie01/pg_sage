package main

import (
	"context"
	"regexp"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/providerobs"
	"github.com/pg-sage/sidecar/internal/rca"
)

var supabaseReference = regexp.MustCompile(`^[a-z0-9]{20}$`)

// Resolve only the project explicitly selected by a supported database endpoint.
// Custom DNS must not cause project discovery with an account-wide token.
func supabaseObservabilityProject(host, user string) string {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	ref := ""
	if strings.HasPrefix(host, "db.") && strings.HasSuffix(host, ".supabase.co") {
		ref = strings.TrimSuffix(strings.TrimPrefix(host, "db."), ".supabase.co")
	} else if hostedProviderFromHost(host) == "supabase" &&
		strings.HasSuffix(host, ".pooler.supabase.com") {
		_, ref, _ = strings.Cut(user, ".")
	}
	if !supabaseReference.MatchString(ref) {
		return ""
	}
	return ref
}

// providerLogRCA reports whether RCA reads a configured provider log sink:
// with logwatch.enabled at auto the sink counts as a detected source
// (rca-09b), and explicit false keeps it off.
func providerLogRCA(cfg *config.Config) bool {
	return cfg.RCA.Enabled && cfg.LogWatch.IsActive(true)
}

func buildProviderObservability(
	pool *pgxpool.Pool, cfg *config.Config, token string,
) (*providerobs.Runtime, *providerobs.LogSink, error) {
	if pool == nil || cfg == nil || strings.TrimSpace(token) == "" {
		return nil, nil, nil
	}
	conn := pool.Config().ConnConfig
	ref := supabaseObservabilityProject(conn.Host, conn.User)
	if ref == "" {
		return nil, nil, nil
	}
	api, err := providerobs.NewSupabase(ref, token)
	if err != nil {
		return nil, nil, err
	}
	sink, err := providerobs.NewLogSink(pool, conn.Database, cfg)
	if err != nil {
		return nil, nil, err
	}
	sink.SetRejectionReporter(func(reason string, count uint64) {
		logWarn("provider", "auto_explain record rejected: %s (count %d)", reason, count)
	})
	var handler providerobs.LogHandler
	if cfg.AutoExplain.Enabled || providerLogRCA(cfg) {
		handler = sink.Handle
	}
	runtime, err := providerobs.NewRuntime(api, conn.Database, handler)
	return runtime, sink, err
}

// startProviderObservability starts Supabase observability when a token
// and a Supabase endpoint select it. It returns the running runtime (nil:
// not started) for the memory gates and the telemetry status, and whether
// it became the RCA engine's log source.
func startProviderObservability(
	ctx context.Context, workers *sync.WaitGroup, pool *pgxpool.Pool,
	cfg *config.Config, exec *executor.Executor, rcaEngine *rca.Engine,
) (*providerobs.Runtime, bool) {
	token, err := config.LookupSecretEnv("SAGE_SUPABASE_OBSERVABILITY_TOKEN")
	if err != nil {
		logWarn("provider", "observability unavailable: %v", err)
		return nil, false
	}
	runtime, sink, err := buildProviderObservability(pool, cfg, token)
	if err != nil {
		logWarn("provider", "observability unavailable: %v", err)
		return nil, false
	}
	if runtime == nil {
		return nil, false
	}
	if err := sink.Start(ctx); err != nil {
		logWarn("provider", "observability log source unavailable: %v", err)
		return nil, false
	}
	runtime.SetRejectionReporter(func(rejected int, total uint64) {
		logWarn("provider", "skipped %d malformed Supabase log record(s) this poll "+
			"(%d since start)", rejected, total)
	})
	if exec != nil {
		exec.WithHostCPUReader(runtime)
	}
	logs := rcaEngine != nil && providerLogRCA(cfg)
	if logs {
		rcaEngine.SetLogSource(sink)
	}
	startInstanceWorker(workers, func() {
		defer sink.Stop()
		runtime.Run(ctx, func(err error) {
			logWarn("provider", "observability collection failed: %v", err)
		})
	})
	return runtime, logs
}

func waitProviderWorkers(ctx context.Context, workers *sync.WaitGroup) bool {
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}
