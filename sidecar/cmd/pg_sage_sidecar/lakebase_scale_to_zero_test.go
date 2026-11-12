package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/testdb"
)

const scaleTestEndpoint = "projects/p/branches/b/endpoints/a"

// fakeScaleToZero replaces the databricksauth endpoint store: host a.example
// maps to scaleTestEndpoint with info, and notes are deduplicated as the real
// store does.
func fakeScaleToZero(t *testing.T, info databricksauth.EndpointInfo) {
	t.Helper()
	noted := map[string]bool{}
	oldInfo, oldNote := lakebaseEndpointInfo, noteScaleToZeroBlockers
	lakebaseEndpointInfo = func(host string) (string, databricksauth.EndpointInfo, bool) {
		if host != "a.example" {
			return "", databricksauth.EndpointInfo{}, false
		}
		return scaleTestEndpoint, info, true
	}
	noteScaleToZeroBlockers = func(endpoint string, blockers []string) []string {
		var added []string
		for _, b := range blockers {
			if !noted[endpoint+b] {
				noted[endpoint+b] = true
				added = append(added, b)
			}
		}
		return added
	}
	t.Cleanup(func() { lakebaseEndpointInfo, noteScaleToZeroBlockers = oldInfo, oldNote })
}

func monitoredPoolConfig(t *testing.T, host string) *pgxpool.Config {
	t.Helper()
	pc, err := pgxpool.ParseConfig("postgres://sage@" + host + "/db")
	if err != nil {
		t.Fatal(err)
	}
	tuneMonitoredPool(pc)
	return pc
}

// Happy path, then state: the monitored pool's defaults are named once; a
// reconnect of the same endpoint does not warn again.
func TestScaleToZeroWarningNamesMonitoredPoolCadences(t *testing.T) {
	fakeScaleToZero(t, databricksauth.EndpointInfo{SuspendTimeout: 5 * time.Minute})
	msg := scaleToZeroWarning(monitoredPoolConfig(t, "a.example"), time.Minute)
	for _, want := range []string{scaleTestEndpoint, "5m0s", "collector every 1m0s",
		"health check every 30s", "min_conns=1", "docs/lakebase.md#cost-model"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("warning %q lacks %q", msg, want)
		}
	}
	if again := scaleToZeroWarning(monitoredPoolConfig(t, "a.example"), time.Minute); again != "" {
		t.Fatalf("second pool on the same endpoint warned again: %q", again)
	}
}

// Nil/empty: an ordinary host, scale-to-zero off, and a nil pool are silent.
func TestScaleToZeroWarningSilentWithoutScaleToZero(t *testing.T) {
	fakeScaleToZero(t, databricksauth.EndpointInfo{})
	if msg := scaleToZeroWarning(monitoredPoolConfig(t, "a.example"), time.Minute); msg != "" {
		t.Fatalf("scale-to-zero off warned: %q", msg)
	}
	fakeScaleToZero(t, databricksauth.EndpointInfo{SuspendTimeout: time.Hour})
	if msg := scaleToZeroWarning(monitoredPoolConfig(t, "db.example"), time.Minute); msg != "" {
		t.Fatalf("ordinary PostgreSQL host warned: %q", msg)
	}
	warnLakebaseScaleToZero("connect", nil, time.Minute) // must not panic
}

// Boundary: the meta-db pool runs no collector (0), so only its pool
// settings are named.
func TestScaleToZeroWarningWithoutCollector(t *testing.T) {
	fakeScaleToZero(t, databricksauth.EndpointInfo{SuspendTimeout: 5 * time.Minute})
	pc, err := pgxpool.ParseConfig("postgres://sage@a.example/meta")
	if err != nil {
		t.Fatal(err)
	}
	pc.MinConns = 1
	msg := scaleToZeroWarning(pc, 0)
	if strings.Contains(msg, "collector") || !strings.Contains(msg, "min_conns=1") ||
		!strings.Contains(msg, "health check every 1m0s") {
		t.Fatalf("meta-db warning = %q; want pool settings only", msg)
	}
}

// No concurrent access test here: the store dedupe is tested concurrently in databricksauth.

// Defaults: the collector cadence falls back to the shipped default when the
// config is absent or leaves it at 0.
func TestMonitoredCollectorInterval(t *testing.T) {
	old := cfg
	t.Cleanup(func() { cfg = old })
	cfg = nil
	if got := monitoredCollectorInterval(); got != config.DefaultCollectorInterval {
		t.Fatalf("nil config = %s; want the default", got)
	}
	cfg = &config.Config{}
	if got := monitoredCollectorInterval(); got != config.DefaultCollectorInterval {
		t.Fatalf("zero interval = %s; want the default", got)
	}
	cfg.Collector.IntervalSeconds = 120
	if got := monitoredCollectorInterval(); got != 2*time.Minute {
		t.Fatalf("120 s config = %s; want 2m0s", got)
	}
}

// Integration: each real startup opener, on live PostgreSQL, notes the
// cadences of the pool it actually opened: the monitored and fleet pools with
// the collector, the meta-db pool without it.
func TestStartupOpenersNoteTheirPoolCadences(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	host := fleetConfigFor(dsn).Host
	want := map[string][]string{
		"monitored": {"collector every 1m0s", "health check every 30s",
			"min_conns=1 keeps a connection open"},
		"fleet": {"collector every 1m0s", "health check every 1m0s",
			"min_conns=1 keeps a connection open"},
		"meta": {"health check every 1m0s", "min_conns=1 keeps a connection open"},
	}
	for name, open := range startupOpeners(dsn) {
		t.Run(name, func(t *testing.T) {
			installFakeLakebase(t)
			var noted []string
			oldInfo, oldNote := lakebaseEndpointInfo, noteScaleToZeroBlockers
			lakebaseEndpointInfo = func(h string) (string, databricksauth.EndpointInfo, bool) {
				return scaleTestEndpoint, databricksauth.EndpointInfo{SuspendTimeout: time.Hour},
					h == host
			}
			noteScaleToZeroBlockers = func(_ string, blockers []string) []string {
				noted = append(noted, blockers...)
				return nil
			}
			t.Cleanup(func() { lakebaseEndpointInfo, noteScaleToZeroBlockers = oldInfo, oldNote })
			pool, err := open()
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			pool.Close()
			if !reflect.DeepEqual(noted, want[name]) {
				t.Fatalf("noted %q; want %q", noted, want[name])
			}
		})
	}
}
