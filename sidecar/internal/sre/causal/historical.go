package causal

import (
	"fmt"
	"slices"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// WithHistorical consumes historical RCA/change context without altering live
// roots, confidence, incident state or customer-impact claims. Each bounded fact
// retains event time and identity. Missing/empty samples never refute a cause.
func WithHistorical(d Diagnosis, obs []Observation) Diagnosis {
	for _, id := range probes.HistoricalIDs() {
		o, ok := find(obs, id)
		if !ok {
			continue
		}
		rows, err := probes.HistoricalRows(o.Result)
		if err != nil || len(rows) == 0 {
			missing := Missing{ProbeID: id, Status: o.Result.Status, Reason: "historical observations unavailable; capture completeness unknown"}
			if !slices.Contains(d.Missing, missing) {
				d.Missing = append(d.Missing, missing)
			}
			continue
		}
		for i, r := range rows {
			if i == maxChangeFacts {
				break
			}
			prefix := fmt.Sprintf("Historical partial observation from %s at %s (event %s; capture completeness and exact multiplicity unknown): ",
				probes.FormatValue(r.Source), r.At.UTC().Format(time.RFC3339Nano), probes.FormatValue(r.EventID))
			var text string
			switch id {
			case probes.LakebaseLogs:
				text = "provider log observed"
				if slices.Contains(historicalLogSignals, r.Signal) {
					text = "RCA classifier observed " + probes.FormatValue(r.Signal) + "; current occurrence and session identity are unknown"
				}
			case probes.LakebaseChanges:
				text = "DDL change " + probes.FormatValue(r.Command) + " (" + probes.FormatValue(r.ObjectType) + "); incident correlation and causal mechanism are unproven"
			case probes.LakebasePlans:
				text = "query " + probes.FormatValue(r.QueryID) + " plan capture observed; regression and present workload impact are unproven"
				if r.PlanHash != "" {
					text += "; retained structured plan hash " + probes.FormatValue(r.PlanHash)
				}
			case probes.LakebaseSessions:
				text = "session activity sampled; session identity and present blocking state are unknown"
			case probes.LakebaseDatabase:
				text = "database interval metrics observed; values are historical intervals, not live counter deltas or a complete SLO denominator"
			}
			fact := Fact{EvidenceID: o.EvidenceID, Text: prefix + text}
			if !slices.Contains(d.Observed, fact) {
				d.Observed = append(d.Observed, fact)
			}
		}
	}
	return d
}

var historicalLogSignals = []string{
	"log_deadlock_detected", "log_connection_refused", "log_out_of_memory", "log_disk_full", "log_panic_server_crash",
	"log_data_corruption", "log_txid_wraparound_warning", "log_archive_failed", "log_temp_file_created", "log_checkpoint_too_frequent",
	"log_lock_timeout", "log_statement_timeout", "log_replication_conflict", "log_wal_segment_removed", "log_autovacuum_cancel",
	"log_replication_slot_inactive", "log_authentication_failure", "log_slow_query",
}
