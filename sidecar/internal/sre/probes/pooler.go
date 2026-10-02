package probes

import "fmt"

// Pooler telemetry (CHECK-04) is a signal probe: PgBouncer's admin console
// read in Go (internal/sre/pooler), outside the SQL catalog, typed like
// every probe result and never offered to the model. Each row is one
// pool, or one pooler that could not be read (unreachable: true, with a
// reason).
const PoolerPools ID = "pooler_pools"

// PoolerAdminDatabase is PgBouncer's admin console pseudo-database.
const PoolerAdminDatabase = "pgbouncer"

// PoolerPool is one PgBouncer pool (SHOW POOLS joined with SHOW STATS).
// Unknown numbers are NaN.
type PoolerPool struct {
	Pooler        string
	Database      string
	User          string
	PoolMode      string
	ClientActive  float64
	ClientWaiting float64
	ServerActive  float64
	ServerIdle    float64
	ServerUsed    float64
	MaxWaitS      float64 // the oldest waiting client's wait
	AvgWaitUS     float64 // average client wait over PgBouncer's stats period
}

// AdminConsole reports whether the pool is PgBouncer's own console.
func (p PoolerPool) AdminConsole() bool { return p.Database == PoolerAdminDatabase }

// PoolerFailure is a pooler whose telemetry could not be read.
type PoolerFailure struct {
	Pooler string
	Reason string
}

// PoolerPoolsOf decodes a pooler_pools result into its pools and the
// poolers that could not be read.
func PoolerPoolsOf(res Result) ([]PoolerPool, []PoolerFailure, error) {
	rows, err := rowsFor(res, PoolerPools)
	if err != nil {
		return nil, nil, err
	}
	var pools []PoolerPool
	var failures []PoolerFailure
	for i, r := range rows {
		name := strField(r, "pooler")
		if name == "" {
			return nil, nil, fmt.Errorf("pooler_pools row %d: %w", i+1, errNoKey("pooler"))
		}
		if boolField(r, "unreachable") {
			failures = append(failures, PoolerFailure{Pooler: name,
				Reason: strField(r, "reason")})
			continue
		}
		p := PoolerPool{Pooler: name, Database: strField(r, "database"),
			User: strField(r, "user"), PoolMode: strField(r, "pool_mode"),
			ClientActive: floatField(r, "cl_active"), ClientWaiting: floatField(r, "cl_waiting"),
			ServerActive: floatField(r, "sv_active"), ServerIdle: floatField(r, "sv_idle"),
			ServerUsed: floatField(r, "sv_used"), MaxWaitS: floatField(r, "maxwait_s"),
			AvgWaitUS: floatField(r, "avg_wait_us")}
		if p.Database == "" {
			return nil, nil, fmt.Errorf("pooler_pools row %d: %w", i+1, errNoKey("database"))
		}
		pools = append(pools, p)
	}
	return pools, failures, nil
}
