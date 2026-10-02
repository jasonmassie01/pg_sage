package srebench

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Rules-only baselines for the runway families: naive first-match rules
// with no nearness, consumption, cycling or pinning checks, so the
// graph's decoys show what those checks add.

func rowsResult(id probes.ID, at time.Duration, rows ...probes.Row) probes.Result {
	st := probes.StatusOK
	if len(rows) == 0 {
		st = probes.StatusEmpty
	}
	return probes.Result{ProbeID: id, Status: st, ObservedAt: t0.Add(at), Rows: rows}
}

func TestRulesOnly_Sequence(t *testing.T) {
	seq := func(fraction float64, owner any) probes.Result {
		return rowsResult(probes.SequenceRunwayProbe, 0, probes.Row{
			"sequence": "public.s", "data_type": "bigint", "last_value": 9e8,
			"max_value": 9.2e18, "type_max": 9.2e18, "owner_type_max": owner,
			"effective_limit": 2147483647.0, "fraction_used": fraction, "cycle": true})
	}
	cases := []struct {
		name string
		ev   probes.Result
		want string
	}{
		{"near, narrower column (cycling ignored)", seq(0.9, 2147483647.0),
			"column_narrower_than_sequence"},
		{"near, own type", seq(0.9, nil), "sequence_type_limit"},
		{"far", seq(0.1, 2147483647.0), ""},
		{"unreadable", probes.Result{ProbeID: probes.SequenceRunwayProbe,
			Status: probes.StatusNoPrivilege}, ""},
	}
	for _, c := range cases {
		if o := derive(sre.TriggerSequence, "sequence public.s", c.ev); o.Root != c.want {
			t.Errorf("%s: root %q, want %q", c.name, o.Root, c.want)
		}
	}
	if o := derive(sre.TriggerSequence, "sequence public.other", seq(0.9, nil)); o.Root != "" {
		t.Errorf("another sequence's runway named a root: %q", o.Root)
	}
}

func TestRulesOnly_Wraparound(t *testing.T) {
	holders := func(kinds ...string) probes.Result {
		var rows []probes.Row
		for _, k := range kinds {
			rows = append(rows, probes.Row{"holder_kind": k, "xmin_age": int64(10)})
		}
		return rowsResult(probes.XminHorizon, 0, rows...)
	}
	xid := func(workers int64) probes.Result {
		return rowsResult(probes.XIDRunwayProbe, 0, probes.Row{"next_xid": int64(1),
			"autovacuum_on": true, "autovacuum_max_workers": int64(3),
			"autovacuum_workers": workers})
	}
	cases := []struct {
		name string
		ev   []probes.Result
		want string
	}{
		{"prepared first", []probes.Result{holders("session", "prepared_xact")},
			"xmin_held_by_prepared_xact"},
		{"slot", []probes.Result{holders("slot_catalog")}, "xmin_held_by_replication"},
		{"any session", []probes.Result{holders("session")}, "xmin_held_by_session"},
		{"busy workers", []probes.Result{holders(), xid(3)}, "autovacuum_saturated"},
		{"nothing", []probes.Result{holders(), xid(1)}, ""},
	}
	for _, c := range cases {
		if o := derive(sre.TriggerWraparound, "xid", c.ev...); o.Root != c.want {
			t.Errorf("%s: root %q, want %q", c.name, o.Root, c.want)
		}
	}
}

func TestRulesOnly_DiskWAL(t *testing.T) {
	growth := func(rate float64) probes.Result {
		return rowsResult(probes.RunwayTrendsProbe, 0, probes.Row{"kind": "database_bytes",
			"subject": "cluster", "samples": int64(4), "rate_per_s": rate, "r2": 0.1})
	}
	inactive := rowsResult(probes.ReplicationSlots, 0, probes.Row{"slot_name": "s",
		"active": false, "retained_bytes": int64(1 << 20)})
	cases := []struct {
		name string
		ev   []probes.Result
		want string
	}{
		{"inactive slot first", []probes.Result{inactive, growth(1e6)}, "inactive_slot"},
		{"any growth (churn included)", []probes.Result{growth(5)}, "database_growth"},
		{"flat", []probes.Result{growth(0)}, ""},
	}
	for _, c := range cases {
		if o := derive(sre.TriggerDiskWAL, "disk", c.ev...); o.Root != c.want {
			t.Errorf("%s: root %q, want %q", c.name, o.Root, c.want)
		}
	}
}
