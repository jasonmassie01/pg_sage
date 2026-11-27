package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// clusterLogDrops is one cluster's log watcher loss since it started.
type clusterLogDrops struct {
	Cluster string
	Counts  logwatch.DropCounts
}

// droppedLines lists every running cluster watcher's losses, by cluster.
func (s *logFanoutSet) droppedLines() []clusterLogDrops {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]clusterLogDrops, 0, len(s.clusters))
	for key, cl := range s.clusters {
		if !cl.active {
			continue
		}
		out = append(out, clusterLogDrops{Cluster: key, Counts: cl.fanout.DroppedLines()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cluster < out[j].Cluster })
	return out
}

// writeLogwatchMetrics emits the log lines each cluster's watcher lost
// (rca-06): evicted from the full line queue (critical lines are kept
// first), skipped by logwatch.max_lines_per_cycle, or over their size
// limit (rca-04). Any rise means log-based detection may have missed events.
func writeLogwatchMetrics(b *strings.Builder, drops []clusterLogDrops) {
	metricsreg.Header(b, "pg_sage_logwatch_dropped_lines_total")
	for _, d := range drops {
		fmt.Fprintf(b, "pg_sage_logwatch_dropped_lines_total{cluster=%q,reason=%q} %d\n",
			d.Cluster, logwatch.DropReasonQueueOverflow, d.Counts.QueueOverflow)
		fmt.Fprintf(b, "pg_sage_logwatch_dropped_lines_total{cluster=%q,reason=%q} %d\n",
			d.Cluster, logwatch.DropReasonClassifierCap, d.Counts.ClassifierCap)
		fmt.Fprintf(b, "pg_sage_logwatch_dropped_lines_total{cluster=%q,reason=%q} %d\n",
			d.Cluster, logwatch.DropReasonOversized, d.Counts.Oversized)
	}
	b.WriteString("\n")
}
