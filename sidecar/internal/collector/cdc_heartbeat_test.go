package collector

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// market-13: replication.cdc_heartbeats. For a configured slot that is a
// logical slot of this database, pg_sage runs one SELECT max(column) on the
// heartbeat table under a 2 s statement_timeout. With nothing configured,
// nothing runs.
//
// No concurrent-access tests: the step runs inside the single collector
// goroutine and shares nothing.

func TestCDCHeartbeatSQL_QuotesIdentifiers(t *testing.T) {
	for _, tc := range []struct{ table, column, want string }{
		{"cdc.heartbeat", "ts", `max("ts")::timestamptz FROM "cdc"."heartbeat"`},
		{"heartbeat", "beat_at", `max("beat_at")::timestamptz FROM "heartbeat"`},
		{`we"ird`, "ts", `FROM "we""ird"`},
		{"cdc.hb; DROP TABLE x", "ts", `FROM "cdc"."hb; drop table x"`},
		// plain identifiers fold to lower case, as they do unquoted in SQL
		{"CDC.HeartBeat", "TS", `max("ts")::timestamptz FROM "cdc"."heartbeat"`},
	} {
		sql, err := cdcHeartbeatSQL(tc.table, tc.column)
		if err != nil {
			t.Fatalf("%s.%s: %v", tc.table, tc.column, err)
		}
		if !strings.Contains(sql, tc.want) || !strings.HasPrefix(sql, sageTag) {
			t.Errorf("%s.%s: sql = %q, want it to contain %q", tc.table, tc.column, sql, tc.want)
		}
	}
	for _, bad := range []struct{ table, column string }{
		{"", "ts"}, {"a.b.c", "ts"}, {"cdc.", "ts"}, {".hb", "ts"}, {"cdc.hb", ""},
	} {
		if _, err := cdcHeartbeatSQL(bad.table, bad.column); err == nil {
			t.Errorf("%q.%q: want an error", bad.table, bad.column)
		}
	}
}

// The read is bounded at 2 s whatever safety.query_timeout_ms says, and
// keeps the configured lock timeout.
func TestCDCHeartbeatTimeouts(t *testing.T) {
	cfg := testConfig()
	cfg.Safety.QueryTimeoutMs = 30000
	tm := heartbeatTimeouts(cfg)
	if tm.Statement != 2*time.Second {
		t.Errorf("statement timeout = %s, want 2s", tm.Statement)
	}
	if tm.Lock <= 0 || tm.Lock > 2*time.Second {
		t.Errorf("lock timeout = %s, want bounded by the statement timeout", tm.Lock)
	}
	cfg.Safety.QueryTimeoutMs = 500
	if tm := heartbeatTimeouts(cfg); tm.Statement != 500*time.Millisecond {
		t.Errorf("a shorter configured timeout must win: %s", tm.Statement)
	}
}

func heartbeatServer(t *testing.T) (*Collector, string, func(...config.CDCHeartbeat)) {
	t.Helper()
	version, create := slotServer(t)
	pool := testPool(t)
	ctx := context.Background()
	n := time.Now().UnixNano()
	slot, table := fmt.Sprintf("m13_hb_slot_%d", n), fmt.Sprintf("m13_hb_%d", n)
	create(slot)
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE public.%s (ts timestamptz);
		INSERT INTO public.%s VALUES (now() - interval '3 minutes'),
		(now() - interval '1 hour')`, table, table)); err != nil {
		t.Fatalf("create heartbeat table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+table)
	})
	cfg := testConfig()
	c := New(pool, cfg, version, noopLog)
	set := func(hb ...config.CDCHeartbeat) { cfg.Replication.CDCHeartbeats = hb }
	return c, table, set
}

func TestCollectCDCHeartbeats_ReadsNewestValue(t *testing.T) {
	c, table, set := heartbeatServer(t)
	slot := strings.Replace(table, "m13_hb_", "m13_hb_slot_", 1)
	set(config.CDCHeartbeat{Slot: slot, Table: "public." + table, Column: "ts",
		MaxAgeMinutes: 1},
		config.CDCHeartbeat{Slot: "m13_no_such_slot", Table: "public." + table,
			Column: "ts", MaxAgeMinutes: 1},
		config.CDCHeartbeat{Slot: slot + "x", Table: "public.nope", Column: "ts",
			MaxAgeMinutes: 1})
	snap := &Snapshot{}
	retries := 0
	c.collectExtras(context.Background(), snap, &retries)
	if len(snap.CDCHeartbeats) != 1 {
		t.Fatalf("readings = %+v, want only the slot that exists here", snap.CDCHeartbeats)
	}
	r := snap.CDCHeartbeats[0]
	if r.Err != "" || r.Latest == nil || r.Slot != slot || r.MaxAge != time.Minute {
		t.Fatalf("reading = %+v, want the newest value and no error", r)
	}
	age := time.Since(*r.Latest)
	if age < 2*time.Minute || age > 5*time.Minute {
		t.Fatalf("newest heartbeat is %s old, want about 3 minutes (max, not min)", age)
	}
}

// A heartbeat configured in upper or mixed case reads the table PostgreSQL
// would resolve for the same unquoted name, not a quoted mixed-case one.
func TestCollectCDCHeartbeats_UnquotedNamesFoldToLowerCase(t *testing.T) {
	c, table, set := heartbeatServer(t)
	slot := strings.Replace(table, "m13_hb_", "m13_hb_slot_", 1)
	set(config.CDCHeartbeat{Slot: slot, Table: "PUBLIC." + strings.ToUpper(table),
		Column: "Ts", MaxAgeMinutes: 1})
	snap := &Snapshot{}
	retries := 0
	c.collectExtras(context.Background(), snap, &retries)
	if len(snap.CDCHeartbeats) != 1 || snap.CDCHeartbeats[0].Err != "" ||
		snap.CDCHeartbeats[0].Latest == nil {
		t.Fatalf("readings = %+v, want the lower-case table read", snap.CDCHeartbeats)
	}
}

// Error propagation: a missing heartbeat table is that heartbeat's error,
// not the snapshot's; the others are still read.
func TestCollectCDCHeartbeats_MissingTableIsThatReadingsError(t *testing.T) {
	c, table, set := heartbeatServer(t)
	slot := strings.Replace(table, "m13_hb_", "m13_hb_slot_", 1)
	set(config.CDCHeartbeat{Slot: slot, Table: "public.m13_missing", Column: "ts",
		MaxAgeMinutes: 1})
	snap := &Snapshot{}
	retries := 0
	c.collectExtras(context.Background(), snap, &retries)
	if len(snap.CDCHeartbeats) != 1 || snap.CDCHeartbeats[0].Latest != nil ||
		!strings.Contains(snap.CDCHeartbeats[0].Err, "does not exist") {
		t.Fatalf("readings = %+v, want one reading with a does-not-exist error",
			snap.CDCHeartbeats)
	}
	if !snap.Available("replication") {
		t.Fatalf("replication lost: %v", snap.Unavailable)
	}
}

// Nil/empty: nothing configured, nothing runs and nothing is reported.
func TestCollectCDCHeartbeats_NoneConfiguredRunsNothing(t *testing.T) {
	c, _, set := heartbeatServer(t)
	set()
	snap := &Snapshot{}
	retries := 0
	c.collectExtras(context.Background(), snap, &retries)
	if snap.CDCHeartbeats != nil {
		t.Fatalf("readings = %+v, want nil", snap.CDCHeartbeats)
	}
	for _, st := range c.extraSteps() {
		if st.name == "cdc_heartbeats" {
			t.Fatal("cdc_heartbeats step registered with nothing configured")
		}
	}
}

// When the slot list could not be read, a configured heartbeat cannot be
// placed: it is reported as unknown, never silently dropped.
func TestCDCHeartbeatReadings_SlotsUnknown(t *testing.T) {
	cfg := testConfig()
	cfg.Replication.CDCHeartbeats = []config.CDCHeartbeat{{Slot: "s", Table: "hb",
		Column: "ts", MaxAgeMinutes: 1}}
	c := New(nil, cfg, 170000, noopLog)
	snap := &Snapshot{Unavailable: map[string]string{"replication": "timeout"}}
	got := c.collectCDCHeartbeats(context.Background(), snap)
	if len(got) != 1 || got[0].Err == "" || got[0].Latest != nil {
		t.Fatalf("readings = %+v, want one unknown reading", got)
	}
}
