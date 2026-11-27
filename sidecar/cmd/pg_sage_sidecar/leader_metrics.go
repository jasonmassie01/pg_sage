package main

import (
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/pg-sage/sidecar/internal/leader"
	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// governanceElector is the governance jobs' own elector when they run on a
// control pool separate from fleet learning's; nil when they share
// fleetLeader (or election is off). Kept for /metrics only; atomic because
// the governance runtime may start after the metrics server.
var governanceElector atomic.Pointer[leader.Elector]

// writeLeaderMetrics emits, per lease scope, whether this sidecar leads,
// how often that changed (a lapse counts), and the seconds since its last
// successful round trip to the lease store. Nil electors (election
// disabled) and an elector passed twice are skipped; with none left the
// block is omitted.
func writeLeaderMetrics(b *strings.Builder, electors ...*leader.Elector) {
	type row struct {
		s   leader.Status
		age float64
	}
	var rows []row
	seen := map[*leader.Elector]bool{}
	for _, e := range electors {
		if e == nil || seen[e] {
			continue
		}
		seen[e] = true
		rows = append(rows, row{s: e.Status(), age: e.LastRenewAge().Seconds()})
	}
	if len(rows) == 0 {
		return
	}
	metricsreg.Header(b, "pg_sage_leader")
	for _, r := range rows {
		fmt.Fprintf(b, "pg_sage_leader{scope=%q} %d\n", r.s.Scope, boolMetric(r.s.Leader))
	}
	metricsreg.Header(b, "pg_sage_leader_transitions_total")
	for _, r := range rows {
		fmt.Fprintf(b, "pg_sage_leader_transitions_total{scope=%q} %d\n",
			r.s.Scope, r.s.Transitions)
	}
	metricsreg.Header(b, "pg_sage_leader_last_renew_age_seconds")
	for _, r := range rows {
		fmt.Fprintf(b, "pg_sage_leader_last_renew_age_seconds{scope=%q} %.3f\n",
			r.s.Scope, r.age)
	}
	b.WriteString("\n")
}

func boolMetric(v bool) int {
	if v {
		return 1
	}
	return 0
}
