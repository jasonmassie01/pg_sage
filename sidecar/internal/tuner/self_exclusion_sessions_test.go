package tuner

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/selfload"
)

// The work_mem hint divides by the active backends: pg_sage's own active
// sessions are not application concurrency (perf v1.8.3, perf-selfexcl).
// The count is server-wide and other test packages share the server, so
// the test compares the minimum of several samples before and after
// pg_sage's sessions start.
func TestSystemContextActiveBackendsLeaveOutPgSage(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	w := selfload.New(t, tunerTestDSN())
	w.StartApp(t)
	active := func() (float64, error) {
		return float64(fetchSystemContext(ctx, pool).ActiveBackends), nil
	}
	before := selfload.MinOver(t, 8, active)
	w.StartSage(t, 6) // 8 active pg_sage sessions
	after := selfload.MinOver(t, 8, active)
	if before < 2 || after >= before+4 {
		t.Fatalf("active backends %.0f -> %.0f after 8 active pg_sage sessions started, "+
			"want them left out (and the application's 2 counted)", before, after)
	}
}
