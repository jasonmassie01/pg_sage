package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/auditjob"
	"github.com/pg-sage/sidecar/internal/leader"
	"github.com/pg-sage/sidecar/internal/siem"
)

// audit-12 review: a sidecar that loses the fleet lease keeps what it
// recorded as leader until its next scheduled verification (up to
// audit.verify_interval_hours later), and its SIEM exporter keeps the
// lanes of its last pass. Neither may reach /metrics or the status route
// while it follows: only the leader's series are true.

// State transition: leader records, then loses the lease before its next
// verification tick.
func TestFormerLeaderExportsNoAuditHealth(t *testing.T) {
	preserveAuditFenceGlobals(t)
	resetAuditHealth(t)
	old := auditExport.Load()
	t.Cleanup(func() { auditExport.Store(old) })
	auditExport.Store(siem.NewExporter(func() []siem.Source { return nil },
		[]siem.SinkConfig{{Sink: siem.NewHTTPSink("soc", "https://x.invalid", "", time.Second,
			nil)}}, nil, siem.Options{}, func() (siem.Fence, bool) { return siem.Fence{}, false },
		nil))
	setFleetLeader(nil)
	auditjob.DefaultHealth().Record("orders", []auditchain.Report{{Chain: "action_log",
		VerifiedAt: auditT0}}, nil, auditT0)
	if s := auditHealthSnapshot(); len(s.Chains) != 1 || s.VerifiedAt == nil {
		t.Fatalf("leader snapshot = %+v, want the recorded chain", s)
	}
	if got := siemSinkStatuses(); len(got) != 1 {
		t.Fatalf("leader sink statuses = %+v, want soc", got)
	}

	setFleetLeader(leader.NewElector(stubLease{held: false}, "s", "me", time.Minute))
	_ = currentFleetLeader().Tick(context.Background())
	if s := auditHealthSnapshot(); len(s.Chains) != 0 || s.VerifiedAt != nil {
		t.Fatalf("follower snapshot = %+v, want empty", s)
	}
	if got := siemSinkStatuses(); got != nil {
		t.Fatalf("follower sink statuses = %+v, want nil", got)
	}
	var b strings.Builder
	writeAuditHealthFamilies(&b)
	body := b.String()
	for _, series := range []string{"pg_sage_audit_chain_ok{", "pg_sage_siem_delivered_total{",
		"pg_sage_siem_consecutive_failures{", "pg_sage_audit_chain_verify_errors_total{"} {
		if strings.Contains(body, series) {
			t.Errorf("former leader still exports %s:\n%s", series, body)
		}
	}
	if !strings.Contains(body, "# TYPE pg_sage_audit_chain_ok gauge") {
		t.Errorf("follower scrape lost the chain_ok family:\n%s", body)
	}
	deps := auditAPIDeps(nil, nil)
	if deps.Health == nil {
		t.Fatal("API deps carry no health source")
	}
	if s := deps.Health(); len(s.Chains) != 0 {
		t.Fatalf("status route on a follower serves %+v, want nothing verified", s)
	}
}
