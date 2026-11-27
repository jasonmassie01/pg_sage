package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/databricksauth"
)

// Seams over the databricksauth endpoint store; tests replace them.
var (
	lakebaseEndpointInfo    = databricksauth.EndpointInfoForHost
	noteScaleToZeroBlockers = databricksauth.NoteScaleToZeroBlockers
)

// warnLakebaseScaleToZero logs one WARN when a freshly opened pool on a
// Lakebase endpoint with scale-to-zero on runs a cadence shorter than the
// suspend timeout (databricks-04). collector is 0 for a pool the collector
// does not read. It changes nothing: collection goes on as configured.
func warnLakebaseScaleToZero(component string, p *pgxpool.Pool, collector time.Duration) {
	if p == nil {
		return
	}
	if msg := scaleToZeroWarning(p.Config(), collector); msg != "" {
		logWarn(component, "%s", msg)
	}
}

// scaleToZeroWarning returns the warning for pc, or "" for an ordinary host,
// scale-to-zero off, or blockers already reported for the endpoint.
func scaleToZeroWarning(pc *pgxpool.Config, collector time.Duration) string {
	endpoint, info, ok := lakebaseEndpointInfo(pc.ConnConfig.Host)
	if !ok {
		return ""
	}
	blockers := databricksauth.ScaleToZeroBlockers(info, collector, pc.MinConns,
		pc.HealthCheckPeriod)
	added := noteScaleToZeroBlockers(endpoint, blockers)
	if len(added) == 0 {
		return ""
	}
	return fmt.Sprintf("Lakebase endpoint %s suspends after %s of inactivity, but pg_sage "+
		"runs %s; the compute may never scale to zero and bills while it runs. "+
		"See docs/lakebase.md#cost-model", endpoint, info.SuspendTimeout,
		strings.Join(added, ", "))
}

// monitoredCollectorInterval is the collector cadence on a monitored pool.
func monitoredCollectorInterval() time.Duration {
	if cfg != nil && cfg.Collector.IntervalSeconds > 0 {
		return time.Duration(cfg.Collector.IntervalSeconds) * time.Second
	}
	return config.DefaultCollectorInterval
}
