package main

import (
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/pooler"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// poolerSignalsFor is the pooler telemetry signal (CHECK-04) of one
// database: one pooler_pools probe over every configured pooler that
// fronts it, or none. A pooler whose DSN cannot be resolved is skipped
// with a warning naming the pooler, never the DSN.
func poolerSignalsFor(poolers []config.SREPoolerConfig, database string,
	logFn func(string, string, ...any)) []sre.SignalProbe {
	var cfgs []pooler.Config
	for _, p := range poolers {
		if !p.Fronts(database) {
			continue
		}
		dsn, err := p.ResolveDSN()
		if err != nil {
			logFn("WARN", "sre: db %q: pooler %q skipped: %v", database, p.Name, err)
			continue
		}
		cfgs = append(cfgs, pooler.Config{Name: p.Name, DSN: dsn, Pools: p.Pools,
			Timeout: p.Timeout()})
	}
	if len(cfgs) == 0 {
		return nil
	}
	src, err := pooler.New(cfgs, pooler.DialPgBouncer)
	if err != nil {
		logFn("WARN", "sre: db %q: pooler telemetry not wired: %v", database, err)
		return nil
	}
	return []sre.SignalProbe{{ID: probes.PoolerPools, Run: src.Probe}}
}
