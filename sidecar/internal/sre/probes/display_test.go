package probes

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// DisplaySQL (playbooks-04) renders an argument-free catalog probe as the
// standalone SQL a case proposes: the lowest variant, its row limit
// inlined as the spec's MaxRows, the probe's identity comment first.

var positionalParam = regexp.MustCompile(`\$[0-9]`)

// hasLimit reports a LIMIT of exactly n rows (LIMIT 500 is not LIMIT 50).
func hasLimit(sql string, n int) bool {
	return regexp.MustCompile(fmt.Sprintf(`LIMIT %d\b`, n)).MatchString(sql)
}

func TestDisplaySQL_LockGraphIsBoundedAndIdentified(t *testing.T) {
	got, err := DisplaySQL(LockGraph)
	if err != nil {
		t.Fatalf("DisplaySQL(lock_graph): %v", err)
	}
	if !strings.HasPrefix(got, "/* pg_sage sre:lock_graph v2 */") {
		t.Fatalf("lock_graph display SQL lacks its identity comment: %.80s", got)
	}
	if !strings.HasSuffix(got, fmt.Sprintf("LIMIT %d", MaxRows)) {
		t.Fatalf("lock_graph display SQL does not end in LIMIT %d: %s", MaxRows, got)
	}
	for _, want := range []string{"pg_blocking_pids", "current_database()"} {
		if !strings.Contains(got, want) {
			t.Fatalf("lock_graph display SQL lacks %q", want)
		}
	}
	if positionalParam.MatchString(got) {
		t.Fatalf("lock_graph display SQL keeps a positional parameter: %s", got)
	}
}

// Boundary: a capped spec inlines its own (lower) cap, not the ceiling.
func TestDisplaySQL_InlinesTheSpecCap(t *testing.T) {
	for id, rows := range map[ID]int{XminHorizon: 100, WraparoundTablesProbe: 50,
		StatTables: statViewRows, LongTransactions: MaxRows} {
		got, err := DisplaySQL(id)
		if err != nil {
			t.Fatalf("DisplaySQL(%s): %v", id, err)
		}
		if !hasLimit(got, rows) {
			t.Errorf("%s display SQL lacks LIMIT %d", id, rows)
		}
		if strings.Contains(got, "LIMIT $1") {
			t.Errorf("%s display SQL keeps LIMIT $1", id)
		}
	}
}

// The lowest variant runs on every supported server (PG14 and up).
func TestDisplaySQL_TakesTheLowestVariant(t *testing.T) {
	got, err := DisplaySQL(ReplicationSlots)
	if err != nil {
		t.Fatalf("DisplaySQL(replication_slots): %v", err)
	}
	if !strings.Contains(got, "NULL::timestamptz AS inactive_since") ||
		strings.Contains(got, "s.inactive_since") {
		t.Fatalf("replication_slots display SQL is not the PG14 variant: %s", got)
	}
	for _, want := range []string{"retained_bytes", "wal_status", "safe_wal_size"} {
		if !strings.Contains(got, want) {
			t.Fatalf("replication_slots display SQL lacks %s", want)
		}
	}
}

// Invalid input: probes that take arguments, read an extension through a
// run-time schema, or read pg_sage's history are not display SQL.
func TestDisplaySQL_RefusesProbesItCannotRenderStandalone(t *testing.T) {
	cases := map[ID]string{
		BackendIdentity:         "takes arguments",
		PlanRegressions:         "takes arguments",
		SequenceRunwayProbe:     "takes arguments",
		AutovacuumCancellations: "takes arguments",
		TempSpillStatements:     "pg_stat_statements",
		StatStatements:          "pg_stat_statements",
		"no_such_probe":         "not in the catalog",
		"":                      "not in the catalog",
	}
	for id, want := range cases {
		got, err := DisplaySQL(id)
		if err == nil {
			t.Fatalf("DisplaySQL(%q) = %q, want an error", id, got)
		}
		if got != "" {
			t.Errorf("DisplaySQL(%q) returned SQL with its error: %q", id, got)
		}
		if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), string(id)) {
			t.Errorf("DisplaySQL(%q) error = %q, want it to name the probe and %q",
				id, err, want)
		}
	}
}

// Every argument-free, extension-free catalog probe renders, starts with
// its identity comment and keeps no positional parameter.
func TestDisplaySQL_EveryRenderableProbe(t *testing.T) {
	rendered := 0
	for _, id := range Catalog().IDs() {
		spec, _ := Catalog().Spec(id)
		if spec.Args != ArgsNone || spec.Extension != "" || spec.History || spec.QueryScoped {
			continue
		}
		got, err := DisplaySQL(id)
		if err != nil {
			t.Errorf("DisplaySQL(%s): %v", id, err)
			continue
		}
		rendered++
		prefix := fmt.Sprintf("/* pg_sage sre:%s %s */", id, spec.Version)
		if !strings.HasPrefix(got, prefix) {
			t.Errorf("%s display SQL does not start with %q: %.80s", id, prefix, got)
		}
		if positionalParam.MatchString(got) {
			t.Errorf("%s display SQL keeps a positional parameter", id)
		}
		if !hasLimit(got, spec.MaxRows) {
			t.Errorf("%s display SQL lacks LIMIT %d", id, spec.MaxRows)
		}
	}
	if rendered < 10 {
		t.Fatalf("only %d probes rendered; the catalog filter is wrong", rendered)
	}
}

// Concurrent access: DisplaySQL reads the immutable catalog only, so
// parallel callers get identical text.
func TestDisplaySQL_ConcurrentCallersAgree(t *testing.T) {
	want, err := DisplaySQL(XminHorizon)
	if err != nil {
		t.Fatalf("DisplaySQL(xmin_horizon): %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := DisplaySQL(XminHorizon); err != nil || got != want {
				errs <- fmt.Sprintf("got %d bytes, err %v", len(got), err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}

// connection_saturation v4 (playbooks-04): from PostgreSQL 16 the slots
// a normal role cannot use include reserved_connections. Every version's
// variant reads it as an optional setting, so the case diagnostic, which
// displays the lowest variant, counts it on PG16+ too.
func TestConnectionSaturation_V4CountsReservedConnectionsFrom16(t *testing.T) {
	spec, ok := Catalog().Spec(ConnectionSaturation)
	if !ok || spec.Version != "v4" {
		t.Fatalf("connection_saturation spec = %+v, want v4", spec)
	}
	reserved := "current_setting('reserved_connections', true)"
	for _, version := range []int{140000, 150000, 160000, 170000, 180000} {
		v, ok := spec.VariantFor(version)
		if !ok {
			t.Fatalf("no connection_saturation variant for %d", version)
		}
		if !strings.Contains(v.SQL, reserved) {
			t.Errorf("version %d variant does not read %s", version, reserved)
		}
		if !strings.HasPrefix(v.SQL, "/* pg_sage sre:connection_saturation v4 */") {
			t.Errorf("version %d variant identity comment: %.60s", version, v.SQL)
		}
	}
	display, err := DisplaySQL(ConnectionSaturation)
	if err != nil || !strings.Contains(display, reserved) {
		t.Fatalf("connection_saturation display SQL ignores reserved_connections "+
			"(err %v): %s", err, display)
	}
}
