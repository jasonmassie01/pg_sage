package auditjob

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// audit-12 (integration): the health registry records what a real
// verification found. A clean trail is ok; an UPDATE written with the
// chain triggers disabled is found as row_edited; a run on a closed pool
// keeps the previous result and counts the error.
func TestHealthRecordsRealVerification(t *testing.T) {
	pool := fresh(t, "auditjob_health", true)
	ctx := context.Background()
	src := Source{Name: "orders", Pool: pool}
	h := NewHealthRegistry()
	mustExec(t, pool, `INSERT INTO sage.action_log (action_type, sql_executed)
		VALUES ('vacuum', 'VACUUM a'), ('vacuum', 'VACUUM b')`)
	reports, err := Run(ctx, src)
	h.Record(src.Name, reports, err, time.Now())
	if err != nil {
		t.Fatalf("intact run: %v", err)
	}
	clean := chainOf(t, h.Snapshot(), "orders", "action_log")
	if !clean.OK || len(clean.Problems) != 0 || clean.HeadSeq < 2 {
		t.Fatalf("intact action_log health = %+v, want ok at head >= 2", clean)
	}

	mustExec(t, pool, `ALTER TABLE sage.action_log DISABLE TRIGGER USER`)
	mustExec(t, pool, `UPDATE sage.action_log SET sql_executed = 'SELECT 1'
		WHERE sql_executed = 'VACUUM a'`)
	mustExec(t, pool, `ALTER TABLE sage.action_log ENABLE TRIGGER USER`)
	reports, err = Run(ctx, src)
	h.Record(src.Name, reports, err, time.Now())
	if err != nil {
		t.Fatalf("tampered run: %v", err)
	}
	s := h.Snapshot()
	bad := chainOf(t, s, "orders", "action_log")
	if bad.OK || bad.Problems[auditchain.KindRowEdited] < 1 {
		t.Fatalf("tampered action_log health = %+v, want not ok with row_edited", bad)
	}
	if other := chainOf(t, s, "orders", "auth_audit"); !other.OK {
		t.Fatalf("untouched auth_audit health = %+v, want ok", other)
	}

	pool.Close()
	reports, err = Run(ctx, src)
	h.Record(src.Name, reports, err, time.Now())
	if err == nil {
		t.Fatal("closed pool: no error")
	}
	s = h.Snapshot()
	if kept := chainOf(t, s, "orders", "action_log"); kept.OK ||
		!kept.VerifiedAt.Equal(bad.VerifiedAt) {
		t.Fatalf("after a failed run action_log = %+v, want the previous result %+v", kept, bad)
	}
	if n := errorsOf(s, "orders"); n != 1 {
		t.Fatalf("verify errors after a closed pool = %d, want 1", n)
	}
}
