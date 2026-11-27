package autonomy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// vacuum-02: the freeze custodian reads every xmin-horizon holder once per
// scan, attaches a holder to a red table only when it pins it, and makes
// one holder proposal per holder listing every pinned table. No
// concurrency test: one custodian per database scans in a serial loop.

// redRow is a horizon row at XID age age of a 200 maximum (red under the
// 25% buffer from age 150).
func redRow(t *testing.T, table string, age int64, catalog bool) freezeRow {
	t.Helper()
	row, err := scanHorizonRow(adapterRow{values: []any{"public", table, age, int64(200),
		int64(1), int64(100), 0.0, int64(1), int64(1), int64(8192), (*float64)(nil), catalog}})
	if err != nil {
		t.Fatalf("scanHorizonRow: %v", err)
	}
	return row
}

func redRows(t *testing.T, n int, age int64) []freezeRow {
	rows := make([]freezeRow, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, redRow(t, fmt.Sprintf("t%02d", i), age, false))
	}
	return rows
}

func byFeature(proposals []Proposal, feature string) []Proposal {
	var out []Proposal
	for _, p := range proposals {
		if p.Feature == feature {
			out = append(out, p)
		}
	}
	return out
}

// presentDetail answers every session detail read as present, counting
// the reads.
func presentDetail(calls *int) sessionDetailReader {
	return func(_ context.Context, h *freeze.XminBlocker) (bool, error) {
		*calls++
		xid := h.XminAge
		h.BackendXIDAge, h.User = &xid, "app"
		return true, nil
	}
}

func TestFreezeHoldersOneProposalForFiftyRedTables(t *testing.T) {
	c := NewPostgresFreezeCustodian(nil, "orders", 25)
	calls := 0
	c.detail = presentDetail(&calls)
	start := time.Now().Add(-time.Hour).UTC()
	holders := []freeze.XminBlocker{{Kind: freeze.HolderSession, PID: 77, XminAge: 100,
		State: "idle in transaction", BackendStart: start}}
	proposals, err := c.plan(context.Background(), redRows(t, 50, 160), knownRates, holders, false)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	blockers := byFeature(proposals, "freeze_blocker")
	if len(blockers) != 1 || calls != 1 {
		t.Fatalf("holder proposals = %d (detail reads %d), want 1 and 1", len(blockers), calls)
	}
	holder := blockers[0]
	if len(holder.TargetObjects) != 50 || holder.TargetObjects[0] != "public.t00" ||
		holder.TargetObjects[49] != "public.t49" {
		t.Fatalf("holder targets = %v", holder.TargetObjects)
	}
	if holder.SQL != "SELECT pg_terminate_backend(77)" ||
		holder.ResponseKind != string(freeze.ResponseTerminateBlocker) ||
		holder.Evidence["backend_start"] != start.Format(time.RFC3339Nano) ||
		holder.Evidence["pinned_tables"] != 50 || holder.Deadline == nil {
		t.Fatalf("holder proposal = %#v", holder)
	}
	// 100 < 0.9 x 160: freezing still advances every table to the horizon.
	if freezes := byFeature(proposals, "freeze"); len(freezes) != 50 {
		t.Fatalf("freezes = %d, want 50", len(freezes))
	}
	if proposals[0].Feature != "freeze_blocker" {
		t.Fatalf("first proposal = %#v, want the holder (the root cause) first", proposals[0])
	}
}

func TestFreezeHoldersOldHolderSkipsFutileFreezes(t *testing.T) {
	c := NewPostgresFreezeCustodian(nil, "orders", 25)
	holders := []freeze.XminBlocker{{Kind: freeze.HolderPreparedXact, Name: "5d41402a",
		XminAge: 150, State: "prepared"}}
	proposals, err := c.plan(context.Background(), redRows(t, 3, 160), knownRates, holders, false)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(proposals) != 1 {
		t.Fatalf("proposals = %#v, want the escalation only", proposals)
	}
	p := proposals[0]
	if p.Feature != "freeze_blocker" || p.SQL != "" || p.Plan == "" ||
		p.ResponseKind != string(freeze.ResponseEscalateHolder) ||
		!strings.Contains(p.Plan, "5d41402a") || p.Evidence["plan"] != p.Plan ||
		len(p.TargetObjects) != 3 {
		t.Fatalf("escalation = %#v", p)
	}
}

func TestFreezeHoldersNoHoldersOnlyFreezes(t *testing.T) {
	c := NewPostgresFreezeCustodian(nil, "orders", 25)
	for _, holders := range [][]freeze.XminBlocker{nil, {}} {
		proposals, err := c.plan(context.Background(), redRows(t, 4, 160), knownRates,
			holders, false)
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		if len(proposals) != 4 || len(byFeature(proposals, "freeze")) != 4 {
			t.Fatalf("proposals = %#v, want 4 freezes", proposals)
		}
		for _, p := range proposals {
			if p.Evidence["xmin_holder"] != nil {
				t.Fatalf("freeze names a holder without one: %#v", p.Evidence)
			}
		}
	}
	empty, err := c.plan(context.Background(), nil, knownRates, nil, false)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty catalog = %#v, %v", empty, err)
	}
}

func TestFreezeHoldersYoungHolderIsNotAProposal(t *testing.T) {
	c := NewPostgresFreezeCustodian(nil, "orders", 25)
	calls := 0
	c.detail = presentDetail(&calls)
	// 79 < 0.5 x 160: the holder cannot pin the table.
	holders := []freeze.XminBlocker{{Kind: freeze.HolderSession, PID: 5, XminAge: 79}}
	proposals, err := c.plan(context.Background(), redRows(t, 2, 160), knownRates, holders, false)
	if err != nil || len(byFeature(proposals, "freeze_blocker")) != 0 || calls != 0 ||
		len(byFeature(proposals, "freeze")) != 2 {
		t.Fatalf("proposals = %#v (detail reads %d), %v", proposals, calls, err)
	}
}

func TestFreezeHoldersGoneSessionIsNotSignalled(t *testing.T) {
	c := NewPostgresFreezeCustodian(nil, "orders", 25)
	c.detail = func(context.Context, *freeze.XminBlocker) (bool, error) { return false, nil }
	holders := []freeze.XminBlocker{{Kind: freeze.HolderSession, PID: 5, XminAge: 100}}
	proposals, err := c.plan(context.Background(), redRows(t, 2, 160), knownRates, holders, false)
	if err != nil || len(byFeature(proposals, "freeze_blocker")) != 0 ||
		len(byFeature(proposals, "freeze")) != 2 {
		t.Fatalf("proposals = %#v, %v; want freezes only", proposals, err)
	}
}

func TestFreezeHoldersSessionDetailErrorAbortsThePlan(t *testing.T) {
	c := NewPostgresFreezeCustodian(nil, "orders", 25)
	boom := errors.New("connection reset")
	c.detail = func(context.Context, *freeze.XminBlocker) (bool, error) { return false, boom }
	holders := []freeze.XminBlocker{{Kind: freeze.HolderSession, PID: 5, XminAge: 150}}
	_, err := c.plan(context.Background(), redRows(t, 1, 160), knownRates, holders, false)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "pid 5") {
		t.Fatalf("plan error = %v, want the detail error naming pid 5", err)
	}
}

// A slot's catalog_xmin holds back only catalog tables (user_catalog_table).
func TestFreezeHoldersCatalogSlotPinsOnlyCatalogTables(t *testing.T) {
	c := NewPostgresFreezeCustodian(nil, "orders", 25)
	holders := []freeze.XminBlocker{{Kind: freeze.HolderSlotCatalog, Name: "cdc",
		XminAge: 155, State: "inactive"}}
	rows := []freezeRow{redRow(t, "plain", 160, false), redRow(t, "decoded", 160, true)}
	proposals, err := c.plan(context.Background(), rows, knownRates, holders, false)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	blockers, freezes := byFeature(proposals, "freeze_blocker"), byFeature(proposals, "freeze")
	if len(blockers) != 1 || len(blockers[0].TargetObjects) != 1 ||
		blockers[0].TargetObjects[0] != "public.decoded" ||
		!strings.Contains(blockers[0].Plan, `"cdc"`) {
		t.Fatalf("slot escalation = %#v", blockers)
	}
	if len(freezes) != 1 || freezes[0].TargetObjects[0] != "public.plain" {
		t.Fatalf("freezes = %#v, want public.plain only", freezes)
	}
}

func TestFreezeHolderProposalOmitsDeadlineWhenRateUnknown(t *testing.T) {
	c := NewPostgresFreezeCustodian(nil, "orders", 25)
	holders := []freeze.XminBlocker{{Kind: freeze.HolderSlot, Name: "s", XminAge: 150}}
	unknown := freezeRateSample{xid: 1, mxid: 1}
	proposals, err := c.plan(context.Background(), redRows(t, 1, 160), unknown, holders, false)
	if err != nil || len(proposals) != 1 || proposals[0].Deadline != nil {
		t.Fatalf("proposals = %#v, %v; want one escalation without a deadline", proposals, err)
	}
}

func TestFreezeHolderKindsMatchTheProbe(t *testing.T) {
	for freezeKind, probeKind := range map[string]string{
		freeze.HolderSession: probes.HolderSession, freeze.HolderStandby: probes.HolderStandby,
		freeze.HolderPreparedXact: probes.HolderPreparedXact,
		freeze.HolderSlot:         probes.HolderSlot,
		freeze.HolderSlotCatalog:  probes.HolderSlotCatalog,
	} {
		if freezeKind != probeKind {
			t.Fatalf("freeze kind %q != probe kind %q", freezeKind, probeKind)
		}
	}
}

func TestProtectedHolderApplications(t *testing.T) {
	for app, want := range map[string]bool{
		"pg_dump": true, "PG_DUMP 17": true, "pg_basebackup": true, "pg_restore": true,
		"pg_sage-sidecar": true, "psql": false, "": false, "orders-api": false,
	} {
		if got := protectedApplication(app); got != want {
			t.Fatalf("protectedApplication(%q) = %v, want %v", app, got, want)
		}
	}
}
