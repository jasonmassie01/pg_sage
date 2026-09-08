package main

import (
	"context"
	"os"
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
var standaloneProviderWorkers sync.WaitGroup

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
	if cfg.AutoExplain.Enabled || (cfg.LogWatch.Enabled && cfg.RCA.Enabled) {
		handler = sink.Handle
	}
	runtime, err := providerobs.NewRuntime(api, conn.Database, handler)
	return runtime, sink, err
}

func startProviderObservability(
	ctx context.Context, workers *sync.WaitGroup, pool *pgxpool.Pool,
	cfg *config.Config, exec *executor.Executor, rcaEngine *rca.Engine,
) {
	runtime, sink, err := buildProviderObservability(
		pool, cfg, os.Getenv("SAGE_SUPABASE_OBSERVABILITY_TOKEN"))
	if err != nil {
		logWarn("provider", "observability unavailable: %v", err)
		return
	}
	if runtime == nil {
		return
	}
	if err := sink.Start(ctx); err != nil {
		logWarn("provider", "observability log source unavailable: %v", err)
		return
	}
	if exec != nil {
		exec.WithHostLoadReader(runtime)
	}
	if rcaEngine != nil && cfg.LogWatch.Enabled {
		rcaEngine.SetLogSource(sink)
	}
	startInstanceWorker(workers, func() {
		defer sink.Stop()
		runtime.Run(ctx, func(err error) {
			logWarn("provider", "observability collection failed: %v", err)
		})
	})
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
