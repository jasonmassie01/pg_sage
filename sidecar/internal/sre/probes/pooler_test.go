package probes

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

// Pooler telemetry (CHECK-04): one pooler_pools row per PgBouncer pool,
// and one row per pooler that could not be read. Unknown numbers are
// NaN, never zero; an unavailable result is an error.

func TestPoolerPools_DecodesPoolsAndFailures(t *testing.T) {
	res := okResult(PoolerPools, nil,
		Row{"pooler": "pgb-1", "database": "orders", "user": "app",
			"pool_mode": "transaction", "cl_active": int64(20), "cl_waiting": int64(7),
			"sv_active": int64(20), "sv_idle": int64(0), "sv_used": int64(1),
			"maxwait_s": 2.5, "avg_wait_us": json.Number("1200")},
		Row{"pooler": "pgb-2", "unreachable": true, "reason": "pooler_timeout"})
	pools, failures, err := PoolerPoolsOf(res)
	if err != nil || len(pools) != 1 || len(failures) != 1 {
		t.Fatalf("pools %+v failures %+v (%v)", pools, failures, err)
	}
	p := pools[0]
	if p.Pooler != "pgb-1" || p.Database != "orders" || p.User != "app" ||
		p.PoolMode != "transaction" || p.ClientActive != 20 || p.ClientWaiting != 7 ||
		p.ServerActive != 20 || p.ServerIdle != 0 || p.ServerUsed != 1 ||
		p.MaxWaitS != 2.5 || p.AvgWaitUS != 1200 {
		t.Fatalf("pool = %+v", p)
	}
	if failures[0] != (PoolerFailure{Pooler: "pgb-2", Reason: "pooler_timeout"}) {
		t.Fatalf("failure = %+v", failures[0])
	}
}

func TestPoolerPools_UnknownNumbersAreNaN(t *testing.T) {
	pools, _, err := PoolerPoolsOf(okResult(PoolerPools, nil,
		Row{"pooler": "pgb-1", "database": "orders", "user": "app"}))
	if err != nil || len(pools) != 1 {
		t.Fatalf("pools = %+v (%v)", pools, err)
	}
	p := pools[0]
	for name, v := range map[string]float64{"cl_waiting": p.ClientWaiting,
		"sv_idle": p.ServerIdle, "maxwait": p.MaxWaitS, "avg_wait": p.AvgWaitUS} {
		if !math.IsNaN(v) {
			t.Errorf("%s = %v, want NaN (unknown)", name, v)
		}
	}
}

func TestPoolerPools_InvalidAndUnavailable(t *testing.T) {
	if _, _, err := PoolerPoolsOf(okResult(PoolerPools, nil,
		Row{"database": "orders"})); err == nil {
		t.Fatal("a row without its pooler decoded")
	}
	if _, _, err := PoolerPoolsOf(okResult(PoolerPools, nil,
		Row{"pooler": "pgb-1"})); err == nil {
		t.Fatal("a reachable pool row without its database decoded")
	}
	failed := Result{ProbeID: PoolerPools, Status: StatusError, Reason: "pooler_unreachable"}
	var ue *UnavailableError
	if _, _, err := PoolerPoolsOf(failed); !errors.As(err, &ue) {
		t.Fatalf("an unavailable result decoded as %v", err)
	}
	if _, _, err := PoolerPoolsOf(okResult(ConnectionSaturation, nil,
		Row{"pooler": "x"})); err == nil {
		t.Fatal("another probe's result decoded as pooler telemetry")
	}
	pools, failures, err := PoolerPoolsOf(okResult(PoolerPools, nil))
	if err != nil || len(pools) != 0 || len(failures) != 0 {
		t.Fatalf("an empty result = %v %v (%v)", pools, failures, err)
	}
}

// The admin console is PgBouncer's own pseudo-database, never a pool.
func TestPoolerPool_AdminConsole(t *testing.T) {
	if !(PoolerPool{Database: "pgbouncer"}).AdminConsole() ||
		(PoolerPool{Database: "orders"}).AdminConsole() {
		t.Fatal("admin console detection is wrong")
	}
}
