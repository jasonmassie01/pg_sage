package api

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// policy-03: the evidence pack reads the standing policy from the control
// database, where the API writes it, not from the audited fleet database.
func TestAuditEvidenceReadsThePolicyFromTheControlPool(t *testing.T) {
	control, audited := &pgxpool.Pool{}, &pgxpool.Pool{}
	if got := (AuditDeps{Control: control}).policyPool(audited); got != control {
		t.Fatalf("policy pool = %p, want the control pool %p", got, control)
	}
	if got := (AuditDeps{}).policyPool(audited); got != audited {
		t.Fatalf("policy pool without a control pool = %p, want the audited %p", got, audited)
	}
}
