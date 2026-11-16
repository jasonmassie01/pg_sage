package main

import (
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/inbox"
	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// writeInboxMetrics renders the inbox (ux-01a): the waiting items by kind
// at the last complete fleet-wide read, and the time from approval needed
// to decision by kind and surface.
func writeInboxMetrics(b *strings.Builder) {
	items, decisions := inbox.Snapshot()
	metricsreg.Header(b, "pg_sage_inbox_items")
	if items != nil {
		for _, k := range inbox.Kinds() {
			writeSample(b, "pg_sage_inbox_items", []labelPair{{"kind", string(k)}}, items[k])
		}
	}
	b.WriteString("\n")
	const name = "pg_sage_inbox_decision_seconds"
	metricsreg.Header(b, name)
	for _, s := range decisions {
		labels := []labelPair{{"kind", string(s.Kind)}, {"decided_from", s.Surface}}
		var cumulative uint64
		for i, bound := range inbox.DecisionBounds {
			cumulative += s.Buckets[i]
			writeSample(b, name+"_bucket",
				withLabel(labels, "le", strconv.FormatFloat(bound, 'g', -1, 64)), cumulative)
		}
		writeSample(b, name+"_bucket", withLabel(labels, "le", "+Inf"), s.Count)
		writeSample(b, name+"_sum", labels, s.Sum)
		writeSample(b, name+"_count", labels, s.Count)
	}
	b.WriteString("\n")
}
