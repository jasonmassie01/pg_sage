package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/logwatch"
)

// rca-06: /metrics exposes pg_sage_logwatch_dropped_lines_total by cluster
// and reason.

func TestWriteLogwatchMetrics(t *testing.T) {
	var b strings.Builder
	writeLogwatchMetrics(&b, []clusterLogDrops{
		{Cluster: "db1:5432", Counts: logwatch.DropCounts{QueueOverflow: 10001, ClassifierCap: 3}},
		{Cluster: "db2:5433", Counts: logwatch.DropCounts{}},
	})
	out := b.String()
	for _, want := range []string{
		"# HELP pg_sage_logwatch_dropped_lines_total ",
		"# TYPE pg_sage_logwatch_dropped_lines_total counter\n",
		logwatchSeries("db1:5432", "queue_overflow", 10001),
		logwatchSeries("db1:5432", "classifier_cap", 3),
		logwatchSeries("db2:5433", "queue_overflow", 0),
		logwatchSeries("db2:5433", "classifier_cap", 0),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q:\n%s", want, out)
		}
	}
}

func logwatchSeries(cluster, reason string, n int) string {
	return fmt.Sprintf("pg_sage_logwatch_dropped_lines_total{cluster=%q,reason=%q} %d\n",
		cluster, reason, n)
}

func TestWriteLogwatchMetricsEmpty(t *testing.T) {
	var b strings.Builder
	writeLogwatchMetrics(&b, nil)
	out := b.String()
	if !strings.Contains(out, "# TYPE pg_sage_logwatch_dropped_lines_total counter") {
		t.Fatalf("empty metrics lack the TYPE line:\n%s", out)
	}
	if strings.Contains(out, "pg_sage_logwatch_dropped_lines_total{") {
		t.Fatalf("empty metrics emitted a series:\n%s", out)
	}
}

// A label value with quotes is escaped, never breaking the exposition.
func TestWriteLogwatchMetricsEscapesCluster(t *testing.T) {
	var b strings.Builder
	writeLogwatchMetrics(&b, []clusterLogDrops{{Cluster: `h"x:1`}})
	if !strings.Contains(b.String(), `cluster="h\"x:1"`) {
		t.Fatalf("cluster label not escaped:\n%s", b.String())
	}
}

func writeStormLog(t *testing.T, dir string, lines int) {
	t.Helper()
	var buf bytes.Buffer
	for i := 0; i < lines; i++ {
		rec, err := json.Marshal(map[string]any{
			"timestamp": "2026-11-08 12:00:00.000 UTC", "pid": i + 1,
			"error_severity": "ERROR", "state_code": "42P01",
			"message": fmt.Sprintf("relation t%d does not exist", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(rec)
		buf.WriteByte('\n')
	}
	f, err := os.OpenFile(filepath.Join(dir, "postgresql.json"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
}

// CHECK-19: a real watcher's evictions reach /metrics, sorted by cluster.
func TestLogwatchDropsReachMetricsEndpoint(t *testing.T) {
	preserveValueGlobals(t)
	installValueFleet(nil)
	oldSet := runtimeLogFanouts
	t.Cleanup(func() { runtimeLogFanouts = oldSet })
	runtimeLogFanouts = newLogFanoutSet()

	dir := t.TempDir()
	cfg.LogWatch = config.LogWatchConfig{Enabled: logWatchSwitch(true), LogDirectory: dir,
		Format: "jsonlog", PollIntervalMs: 3600000, MaxLineLenBytes: 65536}
	if err := os.WriteFile(filepath.Join(dir, "postgresql.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fanout := runtimeLogFanouts.forCluster(ctx, "zz:5432", nil)
	if fanout == nil {
		t.Fatal("watcher did not start")
	}
	writeStormLog(t, dir, 10012) // appended after the startup lookback
	fanout.DrainSource()

	drops := runtimeLogFanouts.droppedLines()
	if len(drops) != 1 || drops[0].Cluster != "zz:5432" ||
		drops[0].Counts.QueueOverflow < 12 {
		t.Fatalf("droppedLines = %+v, want zz:5432 with >= 12 queue drops", drops)
	}
	body := scrapeMetrics(t)
	want := fmt.Sprintf(
		`pg_sage_logwatch_dropped_lines_total{cluster="zz:5432",reason="queue_overflow"} %d`,
		drops[0].Counts.QueueOverflow)
	if !strings.Contains(body, want) {
		t.Fatalf("/metrics lacks %q", want)
	}
}

// Nil/empty: no watcher means only the metric header.
func TestLogFanoutSetDroppedLinesEmpty(t *testing.T) {
	if got := newLogFanoutSet().droppedLines(); len(got) != 0 {
		t.Fatalf("empty set = %+v", got)
	}
}
