package pgaudit

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

// Boundary: only records that may be a running action are held. Index
// builds and VACUUM are; session SET/RESET, writes and pg_sage's own sage
// schema upkeep are looked up once and dropped, so they never crowd real
// actions out of the hold queue.
func TestIngestHoldsOnlyPossibleActions(t *testing.T) {
	cases := []struct {
		name, msg string
		held      bool
	}{
		{"index build", untaggedDDL, true},
		{"vacuum", `AUDIT: SESSION,1,1,MISC,VACUUM,,,VACUUM (ANALYZE) public.t`, true},
		{"session set", `AUDIT: SESSION,1,1,MISC,SET,,,SET statement_timeout = 5000`, false},
		{"session reset", `AUDIT: SESSION,1,1,MISC,RESET,,,RESET search_path`, false},
		{"write", `AUDIT: SESSION,1,1,WRITE,INSERT,TABLE,public.t,INSERT INTO t VALUES (1)`,
			false},
		{"sage schema", `AUDIT: SESSION,1,1,DDL,CREATE TABLE,TABLE,sage.snapshots,` +
			`CREATE TABLE IF NOT EXISTS sage.snapshots (a int)`, false},
		{"sage index", `AUDIT: SESSION,1,1,DDL,CREATE INDEX,INDEX,sage.idx_snap,` +
			`CREATE INDEX IF NOT EXISTS idx_snap ON sage.snapshots (a)`, false},
	}
	for _, tc := range cases {
		now := time.Now()
		q := &countingQuerier{}
		c := fakeCorrelator(q, &now)
		st, err := c.Ingest(context.Background(),
			[]logwatch.LogEntry{entry("pg_sage", ownRole, tc.msg)})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		wantHeld, wantDropped := 0, 1
		if tc.held {
			wantHeld, wantDropped = 1, 0
		}
		if st.Held != wantHeld || st.Dropped != wantDropped || c.Pending() != wantHeld {
			t.Fatalf("%s: stats %+v pending %d; want held=%d dropped=%d", tc.name, st,
				c.Pending(), wantHeld, wantDropped)
		}
		if queries, _ := q.counts(); queries == 0 {
			t.Fatalf("%s: not looked up at all; a matching action would be missed", tc.name)
		}
	}
}
