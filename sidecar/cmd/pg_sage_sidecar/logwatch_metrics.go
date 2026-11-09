package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/logwatch"
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
	out := make([]clusterLogDrops, 0, len(s.fanouts))
	for key, fanout := range s.fanouts {
		if fanout == nil {
			continue
		}
		out = append(out, clusterLogDrops{Cluster: key, Counts: fanout.DroppedLines()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cluster < out[j].Cluster })
	return out
}

// writeLogwatchMetrics emits the log lines each cluster's watcher lost
// (rca-06): evicted from the full line queue (critical lines are kept
// first) or skipped by logwatch.max_lines_per_cycle. Any rise means
// log-based detection may have missed events.
func writeLogwatchMetrics(b *strings.Builder, drops []clusterLogDrops) {
	b.WriteString("# HELP pg_sage_logwatch_dropped_lines_total PostgreSQL log lines " +
		"the log watcher dropped before classification, by cluster and reason " +
		"(queue_overflow, classifier_cap)\n" +
		"# TYPE pg_sage_logwatch_dropped_lines_total counter\n")
	for _, d := range drops {
		fmt.Fprintf(b, "pg_sage_logwatch_dropped_lines_total{cluster=%q,reason=%q} %d\n",
			d.Cluster, logwatch.DropReasonQueueOverflow, d.Counts.QueueOverflow)
		fmt.Fprintf(b, "pg_sage_logwatch_dropped_lines_total{cluster=%q,reason=%q} %d\n",
			d.Cluster, logwatch.DropReasonClassifierCap, d.Counts.ClassifierCap)
	}
	b.WriteString("\n")
}
