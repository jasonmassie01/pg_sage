package main

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/auditjob"
	"github.com/pg-sage/sidecar/internal/pgaudit"
	"github.com/pg-sage/sidecar/internal/siem"
)

// audit-12: audit health on /metrics, from the scheduled verification's
// registry (auditjob.DefaultHealth) and the running SIEM exporter's
// status. Samples carry names and counts only, never row content.
// Verification runs on the fleet leader only, so a follower exports the
// families' HELP and TYPE with no chain samples.

// writeAuditHealthFamilies renders the process's pgaudit, chain and SIEM
// health on the scrape.
func writeAuditHealthFamilies(b *strings.Builder) {
	writePGAuditMetrics(b, pgaudit.RecordCounts())
	writeAuditMetrics(b, auditjob.DefaultHealth().Snapshot(), siemSinkStatuses())
}

// writeAuditMetrics renders the chain and SIEM health families.
func writeAuditMetrics(b *strings.Builder, h auditjob.HealthSnapshot, sinks []siem.SinkStatus) {
	writeAuditChainMetrics(b, h)
	writeSIEMMetrics(b, sinks)
}

func writeAuditChainMetrics(b *strings.Builder, h auditjob.HealthSnapshot) {
	b.WriteString("# HELP pg_sage_audit_chain_ok 1 if the audit chain's last scheduled " +
		"verification found no problem, 0 if it fails verification\n" +
		"# TYPE pg_sage_audit_chain_ok gauge\n")
	for _, c := range h.Chains {
		ok := 0
		if c.OK {
			ok = 1
		}
		writeSample(b, "pg_sage_audit_chain_ok", chainLabels(c), ok)
	}
	b.WriteString("\n# HELP pg_sage_audit_chain_verified_timestamp_seconds Unix time of " +
		"the audit chain's last finished verification\n" +
		"# TYPE pg_sage_audit_chain_verified_timestamp_seconds gauge\n")
	for _, c := range h.Chains {
		writeSample(b, "pg_sage_audit_chain_verified_timestamp_seconds", chainLabels(c),
			c.VerifiedAt.Unix())
	}
	b.WriteString("\n# HELP pg_sage_audit_chain_problems Problems the audit chain's last " +
		"verification reported, by kind (a failing chain only)\n" +
		"# TYPE pg_sage_audit_chain_problems gauge\n")
	for _, c := range h.Chains {
		for _, kind := range sortedKeys(c.Problems) {
			writeSample(b, "pg_sage_audit_chain_problems",
				withLabel(chainLabels(c), "kind", kind), c.Problems[kind])
		}
	}
	b.WriteString("\n")
	writeVerifyErrorMetrics(b, h)
}

func chainLabels(c auditjob.ChainHealth) []labelPair {
	return []labelPair{{"database", c.Database}, {"chain", c.Chain}}
}

// writeVerifyErrorMetrics counts the verification runs that failed before
// they finished, per database; a database that verified and never failed
// reports 0, so rate() works from its first failure.
func writeVerifyErrorMetrics(b *strings.Builder, h auditjob.HealthSnapshot) {
	b.WriteString("# HELP pg_sage_audit_chain_verify_errors_total Scheduled audit " +
		"chain verifications that failed before they finished (the chains keep their " +
		"previous result)\n# TYPE pg_sage_audit_chain_verify_errors_total counter\n")
	counts := map[string]int64{}
	for _, c := range h.Chains {
		counts[c.Database] += 0
	}
	for _, e := range h.Errors {
		counts[e.Database] = e.Count
	}
	for _, db := range sortedKeys(counts) {
		writeSample(b, "pg_sage_audit_chain_verify_errors_total", dbLabel(db), counts[db])
	}
	b.WriteString("\n")
}

// writeSIEMMetrics renders each sink's lag per lane (head minus saved
// cursor at its last attempt), consecutive failed passes and delivered
// events since start. A lane without a chain (a source whose chains could
// not be listed) has no lag to report.
func writeSIEMMetrics(b *strings.Builder, sinks []siem.SinkStatus) {
	b.WriteString("# HELP pg_sage_siem_lag_links Audit chain links not yet exported to " +
		"the SIEM sink: chain head minus the sink's cursor\n" +
		"# TYPE pg_sage_siem_lag_links gauge\n")
	for _, s := range sinks {
		for _, l := range s.Lanes {
			if l.Chain == "" {
				continue
			}
			writeSample(b, "pg_sage_siem_lag_links", []labelPair{{"sink", s.Name},
				{"source", l.Source}, {"chain", l.Chain}}, l.Lag)
		}
	}
	b.WriteString("\n# HELP pg_sage_siem_consecutive_failures Failed export passes of the " +
		"SIEM sink in a row (0 after a clean pass)\n" +
		"# TYPE pg_sage_siem_consecutive_failures gauge\n")
	for _, s := range sinks {
		writeSample(b, "pg_sage_siem_consecutive_failures", sinkLabel(s.Name),
			s.ConsecutiveFailures)
	}
	b.WriteString("\n# HELP pg_sage_siem_delivered_total Audit events the SIEM sink " +
		"confirmed since the exporter started\n# TYPE pg_sage_siem_delivered_total counter\n")
	for _, s := range sinks {
		writeSample(b, "pg_sage_siem_delivered_total", sinkLabel(s.Name), s.Delivered)
	}
	b.WriteString("\n")
}

func sinkLabel(name string) []labelPair { return []labelPair{{"sink", name}} }

// siemSinkStatuses is the running exporter's sink health; nil without one.
func siemSinkStatuses() []siem.SinkStatus {
	if e := auditExport.Load(); e != nil {
		return e.Status()
	}
	return nil
}
