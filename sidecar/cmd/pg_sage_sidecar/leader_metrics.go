package main

import (
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/pg-sage/sidecar/internal/leader"
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
	b.WriteString("# HELP pg_sage_leader Whether this sidecar holds the scope's lease " +
		"and may run leader-only jobs (1) or follows (0)\n# TYPE pg_sage_leader gauge\n")
	for _, r := range rows {
		fmt.Fprintf(b, "pg_sage_leader{scope=%q} %d\n", r.s.Scope, boolMetric(r.s.Leader))
	}
	b.WriteString("# HELP pg_sage_leader_transitions_total Leader/follower changes of " +
		"this sidecar, lapses included\n# TYPE pg_sage_leader_transitions_total counter\n")
	for _, r := range rows {
		fmt.Fprintf(b, "pg_sage_leader_transitions_total{scope=%q} %d\n",
			r.s.Scope, r.s.Transitions)
	}
	b.WriteString("# HELP pg_sage_leader_last_renew_age_seconds Seconds since the last " +
		"successful lease store round trip (since start when none yet)\n" +
		"# TYPE pg_sage_leader_last_renew_age_seconds gauge\n")
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
