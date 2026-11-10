package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sre-06 CHECK-04: /metrics carries the probe slots' queue wait and
// refusals by database, caller class and limiter.

func TestWriteSREProbeMetrics_SeriesPerCallerAndLimiter(t *testing.T) {
	var b strings.Builder
	writeSREProbeMetrics(&b, []probes.QueueStat{
		{Database: "orders", Caller: probes.CallerInvestigation,
			Limiter: probes.LimiterSidecar, Waits: 3, WaitSeconds: 5.25, Refusals: 1},
		{Database: "orders", Caller: probes.CallerRunway,
			Limiter: probes.LimiterDatabase, Waits: 7, WaitSeconds: 0.5},
	})
	out := b.String()
	for _, want := range []string{
		"# TYPE pg_sage_sre_probe_refusals_total counter\n",
		"# TYPE pg_sage_sre_probe_queue_wait_seconds summary\n",
		`pg_sage_sre_probe_refusals_total{database="orders",caller="investigation",` +
			`limiter="sidecar"} 1` + "\n",
		`pg_sage_sre_probe_refusals_total{database="orders",caller="runway",` +
			`limiter="database"} 0` + "\n",
		`pg_sage_sre_probe_queue_wait_seconds_sum{database="orders",caller="investigation",` +
			`limiter="sidecar"} 5.25` + "\n",
		`pg_sage_sre_probe_queue_wait_seconds_count{database="orders",caller="runway",` +
			`limiter="database"} 7` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q:\n%s", want, out)
		}
	}
}

func TestWriteSREProbeMetrics_EmptyKeepsHelpAndType(t *testing.T) {
	var b strings.Builder
	writeSREProbeMetrics(&b, nil)
	out := b.String()
	if !strings.Contains(out, "# HELP pg_sage_sre_probe_refusals_total ") ||
		!strings.Contains(out, "# HELP pg_sage_sre_probe_queue_wait_seconds ") {
		t.Fatalf("empty metrics lost their HELP lines:\n%s", out)
	}
	if strings.Contains(out, "{") {
		t.Fatalf("empty metrics emitted a series:\n%s", out)
	}
}

func TestWriteSREProbeMetrics_EscapesLabels(t *testing.T) {
	var b strings.Builder
	writeSREProbeMetrics(&b, []probes.QueueStat{{Database: `a"b`,
		Caller: probes.CallerOther, Limiter: probes.LimiterSidecar, Refusals: 2}})
	if !strings.Contains(b.String(), `database="a\"b"`) {
		t.Fatalf("database label not escaped:\n%s", b.String())
	}
}

// CHECK-04 through the handler: a refusal recorded by a real runner
// reaches /metrics with its caller label.
func TestHandleMetrics_ExposesSREProbeRefusals(t *testing.T) {
	preserveValueGlobals(t)
	cfg, pool, fleetMgr = &config.Config{Mode: "fleet"}, nil, nil
	global := probes.NewLimiter(1)
	holder := probes.NewRunner(nil, probes.Catalog(), global)
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_ = holder.WithSlot(context.Background(), func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	r := probes.NewRunner(nil, probes.Catalog(), global)
	r.SetDatabaseName("metrics_handler_db")
	ctx, cancel := context.WithTimeout(probes.WithCaller(context.Background(),
		probes.CallerInvestigation), 50*time.Millisecond)
	err := r.WithSlot(ctx, func(context.Context) error { return nil })
	cancel()
	close(release)
	<-done
	if !errors.Is(err, probes.ErrConcurrencyLimit) {
		t.Fatalf("WithSlot = %v, want a concurrency limit", err)
	}
	want := `pg_sage_sre_probe_refusals_total{database="metrics_handler_db",` +
		`caller="investigation",limiter="sidecar"} 1`
	if out := scrapeMetrics(t); !strings.Contains(out, want) {
		t.Fatalf("metrics missing %q:\n%s", want, out)
	}
}

// A database removed from the fleet (reload, meta delete or rename) takes
// its probe queue series off /metrics; the others stay.
func TestForgetRemovedDatabase_DropsItsProbeSeries(t *testing.T) {
	preserveValueGlobals(t)
	cfg, pool, fleetMgr = &config.Config{Mode: "fleet"}, nil, nil
	for _, name := range []string{"forget_removed_db", "forget_kept_db"} {
		r := probes.NewRunner(nil, probes.Catalog(), probes.NewLimiter(1))
		r.SetDatabaseName(name)
		if err := r.WithSlot(probes.WithCaller(context.Background(), probes.CallerRunway),
			func(context.Context) error { return nil }); err != nil {
			t.Fatalf("WithSlot(%s) = %v", name, err)
		}
	}
	forgetRemovedDatabase("forget_removed_db")
	out := scrapeMetrics(t)
	if strings.Contains(out, `database="forget_removed_db"`) {
		t.Fatalf("metrics still carry the removed database:\n%s", out)
	}
	want := `pg_sage_sre_probe_queue_wait_seconds_count{database="forget_kept_db",` +
		`caller="runway",limiter="sidecar"} 1`
	if !strings.Contains(out, want) {
		t.Fatalf("metrics lost the kept database's series %q:\n%s", want, out)
	}
}
