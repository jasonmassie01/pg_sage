package autonomy

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/custodian/freeze"
)

// vacuum-04 unit tests: the bloat branch of the freeze custodian reads
// the catalog estimate (never the dead-tuple ratio), the size floor and
// the bounded pgstattuple confirmation.
// No concurrency test: one custodian per database runs in a serial loop.

const testGiB = int64(1) << 30

// horizonValues is one probes.FreezeHorizonsSQL row of a green-urgency table.
func horizonValues(table string, dead float64, oid, pages int64,
	expected *float64) adapterRow {
	return adapterRow{values: []any{"public", table, int64(1), int64(100),
		int64(1), int64(100), dead, oid, pages, pages * 8192, expected}}
}

func pagesPtr(v float64) *float64 { return &v }

func TestFreezeCustodianBloatDefaults(t *testing.T) {
	custodian := NewPostgresFreezeCustodian(nil, "orders", 25)
	if custodian.bloatMinBytes != config.DefaultTableBloatMinBytes ||
		config.DefaultTableBloatMinBytes != testGiB {
		t.Fatalf("default bloat floor = %d (config default %d), want 1 GiB",
			custodian.bloatMinBytes, config.DefaultTableBloatMinBytes)
	}
	if custodian.confirmBounds != catalogread.Default() {
		t.Fatalf("default confirmation bounds = %#v", custodian.confirmBounds)
	}
	custodian.WithBloatBounds(0, catalogread.Timeouts{})
	if custodian.bloatMinBytes != testGiB || custodian.confirmBounds != catalogread.Default() {
		t.Fatalf("zero bounds unbounded the probe: floor %d bounds %#v",
			custodian.bloatMinBytes, custodian.confirmBounds)
	}
	bounds := catalogread.Timeouts{Statement: time.Second, Lock: 50 * time.Millisecond}
	custodian.WithBloatBounds(4096, bounds)
	if custodian.bloatMinBytes != 4096 || custodian.confirmBounds != bounds {
		t.Fatalf("explicit bounds = %d %#v", custodian.bloatMinBytes, custodian.confirmBounds)
	}
}

func TestFreezeBloatEstimateParksAsTypedPlan(t *testing.T) {
	custodian := NewPostgresFreezeCustodian(nil, "orders", 25)
	pages := testGiB/8192 + 10
	for _, repack := range []bool{false, true} {
		proposal, err := custodian.scanResponseRow(
			horizonValues("orders", 0.01, 42, pages, pagesPtr(float64(pages)*0.3)),
			knownRates, nil, repack)
		if err != nil {
			t.Fatalf("scanResponseRow: %v", err)
		}
		kind := string(freeze.ResponseParkBloat)
		if repack {
			kind = string(freeze.ResponsePlanRepack)
		}
		if proposal.ResponseKind != kind || proposal.Feature != "bloat_remediation" ||
			proposal.SQL != "" || proposal.Plan == "" || proposal.Deadline != nil {
			t.Fatalf("bloat proposal (repack=%v) = %#v", repack, proposal)
		}
		ratio, _ := proposal.Evidence["bloat_ratio"].(float64)
		if math.Abs(ratio-0.7) > 1e-9 || proposal.Evidence["bloat_source"] != "estimate" ||
			proposal.Evidence["heap_bytes"] != pages*8192 ||
			proposal.Evidence["dead_tuple_ratio"] != 0.01 {
			t.Fatalf("bloat evidence = %#v", proposal.Evidence)
		}
	}
}

func TestFreezeBloatIgnoresDeadTuplesAndSmallTables(t *testing.T) {
	custodian := NewPostgresFreezeCustodian(nil, "orders", 25)
	big := testGiB/8192 + 1
	cases := map[string]adapterRow{
		// lifeos_runtime_schema_meta: 2 rows, 1 dead, one page, analyzed.
		"two rows one dead":            horizonValues("meta", 0.5, 1, 1, nil),
		"high dead ratio, no estimate": horizonValues("big", 0.9, 2, big, nil),
		"high dead ratio, healthy estimate": horizonValues("big", 0.9, 3, big,
			pagesPtr(float64(big)*0.9)),
		"bloated but under the floor": horizonValues("small", 0, 4, big-2,
			pagesPtr(1)),
		"empty table": horizonValues("empty", 0, 5, 0, pagesPtr(0)),
	}
	for name, row := range cases {
		proposal, err := custodian.scanResponseRow(row, knownRates, nil, true)
		if err != nil {
			t.Fatalf("%s: scanResponseRow: %v", name, err)
		}
		if proposal.SQL != "" || proposal.Plan != "" || proposal.ResponseKind != "" {
			t.Fatalf("%s: produced a proposal %#v", name, proposal)
		}
	}
}

type confirmCall struct{ oid int64 }

func recordingConfirm(calls *[]confirmCall, ratio float64,
	err error) bloatConfirmer {
	return func(_ context.Context, oid int64) (float64, error) {
		*calls = append(*calls, confirmCall{oid: oid})
		return ratio, err
	}
}

func bloatRows(t *testing.T,
	rows ...adapterRow) []freezeRow {
	t.Helper()
	out := make([]freezeRow, 0, len(rows))
	for _, row := range rows {
		scanned, err := scanHorizonRow(row)
		if err != nil {
			t.Fatalf("scanHorizonRow: %v", err)
		}
		out = append(out, scanned)
	}
	return out
}

func TestConfirmBloatProbesAtMostThreeLargestCandidates(t *testing.T) {
	custodian := NewPostgresFreezeCustodian(nil, "orders", 25).
		WithBloatBounds(100*8192, catalogread.Default())
	var calls []confirmCall
	custodian.confirm = recordingConfirm(&calls, 0.8, nil)
	rows := bloatRows(t,
		horizonValues("c1", 0, 11, 200, pagesPtr(10)),
		horizonValues("c2", 0, 12, 600, pagesPtr(10)),
		horizonValues("under_floor", 0, 13, 99, pagesPtr(1)),
		horizonValues("c3", 0, 14, 400, pagesPtr(10)),
		horizonValues("healthy", 0, 15, 5000, pagesPtr(4900)),
		horizonValues("never_analyzed", 0, 16, 7000, nil),
		horizonValues("c4", 0, 17, 300, pagesPtr(10)),
		horizonValues("c5", 0, 18, 100, pagesPtr(50)),
	)
	custodian.confirmBloat(context.Background(), rows, true)
	if len(calls) != custodianBloatConfirmPerCycle || custodianBloatConfirmPerCycle != 3 {
		t.Fatalf("confirmations = %#v, want 3", calls)
	}
	for i, want := range []int64{12, 14, 17} {
		if calls[i].oid != want {
			t.Fatalf("confirmation %d probed oid %d, want %d (largest first): %#v",
				i, calls[i].oid, want, calls)
		}
	}
	sources := map[string]string{}
	for _, row := range rows {
		sources[row.sample.Table] = row.bloatSource
	}
	want := map[string]string{"c2": "pgstattuple", "c3": "pgstattuple",
		"c4": "pgstattuple", "c1": "estimate", "c5": "estimate"}
	for table, source := range want {
		if sources[table] != source {
			t.Fatalf("%s bloat_source = %q, want %q (%v)", table, sources[table], source, sources)
		}
	}
}

func TestConfirmBloatSkippedWithoutPgstattuple(t *testing.T) {
	custodian := NewPostgresFreezeCustodian(nil, "orders", 25).
		WithBloatBounds(8192, catalogread.Default())
	var calls []confirmCall
	custodian.confirm = recordingConfirm(&calls, 0.8, nil)
	rows := bloatRows(t, horizonValues("c1", 0, 11, 200, pagesPtr(10)))
	custodian.confirmBloat(context.Background(), rows, false)
	if len(calls) != 0 || rows[0].bloatSource != "estimate" {
		t.Fatalf("probe ran without pgstattuple: calls %#v source %q", calls, rows[0].bloatSource)
	}
	custodian.confirmBloat(context.Background(), nil, true)
	if len(calls) != 0 {
		t.Fatalf("empty scan probed: %#v", calls)
	}
}

func TestConfirmBloatErrorKeepsEstimate(t *testing.T) {
	custodian := NewPostgresFreezeCustodian(nil, "orders", 25).
		WithBloatBounds(8192, catalogread.Default())
	var calls []confirmCall
	custodian.confirm = recordingConfirm(&calls, 0,
		errors.New("ERROR: canceling statement due to statement timeout (SQLSTATE 57014)"))
	rows := bloatRows(t, horizonValues("c1", 0, 11, 200, pagesPtr(40)))
	custodian.confirmBloat(context.Background(), rows, true)
	proposal, _, err := custodian.respond(context.Background(), rows[0], knownRates, nil, false)
	if err != nil {
		t.Fatalf("respond: %v", err)
	}
	ratio, _ := proposal.Evidence["bloat_ratio"].(float64)
	if len(calls) != 1 || proposal.ResponseKind != string(freeze.ResponseParkBloat) ||
		math.Abs(ratio-0.8) > 1e-9 || proposal.Evidence["bloat_source"] != "estimate" {
		t.Fatalf("timeout lost the estimate: calls %d proposal %#v", len(calls), proposal)
	}
	detail, _ := proposal.Evidence["bloat_confirm_error"].(string)
	if !strings.Contains(detail, "57014") {
		t.Fatalf("confirmation error not recorded: %#v", proposal.Evidence)
	}
}

func TestConfirmBloatMeasurementCanWithdrawThePlan(t *testing.T) {
	custodian := NewPostgresFreezeCustodian(nil, "orders", 25).
		WithBloatBounds(8192, catalogread.Default())
	var calls []confirmCall
	custodian.confirm = recordingConfirm(&calls, 0.2, nil)
	rows := bloatRows(t, horizonValues("c1", 0, 11, 200, pagesPtr(40)))
	custodian.confirmBloat(context.Background(), rows, true)
	proposal, _, err := custodian.respond(context.Background(), rows[0], knownRates, nil, false)
	if err != nil || len(calls) != 1 || proposal.Plan != "" || proposal.ResponseKind != "" {
		t.Fatalf("measured 20%% bloat still planned: %#v (%v)", proposal, err)
	}
}

func TestScanHorizonRowRejectsMalformedRow(t *testing.T) {
	if _, err := scanHorizonRow(failingRow{err: errors.New("conn reset")}); err == nil ||
		!strings.Contains(err.Error(), "scan freeze response") ||
		!strings.Contains(err.Error(), "conn reset") {
		t.Fatalf("scan error = %v", err)
	}
}

type failingRow struct{ err error }

func (r failingRow) Scan(...any) error { return r.err }

// scanResponseRow scans one probes.FreezeHorizonsSQL row and answers it, without
// the per-cycle bloat confirmation (unit tests of the response ladder).
func (c *PostgresFreezeCustodian) scanResponseRow(
	row interface{ Scan(...any) error }, rates freezeRateSample,
	holders []freeze.XminBlocker, repack bool,
) (Proposal, error) {
	scanned, err := scanHorizonRow(row)
	if err != nil {
		return Proposal{}, err
	}
	proposal, _, err := c.respond(context.Background(), scanned, rates, holders, repack)
	return proposal, err
}
