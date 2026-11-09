package freeze

import (
	"strings"
	"testing"
	"time"
)

// vacuum-02: a red table is frozen unless the oldest xmin holder that
// applies to it pins it (holder age at least PinShare of the table's XID
// age); a pinning holder gets its own proposal, and the freeze still runs
// while the holder is younger than FreezeStillHelpsShare of the table age.
// No concurrency test: these are pure functions of their input.

func redInput(tableAge int64, holder *XminBlocker) ResponseInput {
	return ResponseInput{Schema: "public", Table: "orders", Urgency: UrgencyRed,
		XIDAge: tableAge, Holder: holder}
}

func session(pid int, age int64) *XminBlocker {
	return &XminBlocker{Kind: HolderSession, PID: pid, XminAge: age, State: "active"}
}

func TestPlanResponsePinBoundaries(t *testing.T) {
	cases := []struct {
		name           string
		tableAge, age  int64
		freeze, pinned bool
	}{
		{"exactly half pins", 1000, 500, true, true},
		{"just under half only freezes", 1000, 490, true, false},
		{"1.5B table, 0.8B holder: freeze and holder", 1_500_000_000, 800_000_000, true, true},
		{"1.5B table, 1.4B holder: holder only", 1_500_000_000, 1_400_000_000, false, true},
		{"exactly 0.9 of the table: holder only", 1000, 900, false, true},
		{"just under 0.9 still freezes", 1000, 899, true, true},
		{"holder older than the table", 1000, 5000, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			response, err := PlanResponse(redInput(c.tableAge, session(7, c.age)))
			if err != nil {
				t.Fatalf("PlanResponse: %v", err)
			}
			froze := response.Kind == ResponseVacuumFreeze &&
				strings.HasPrefix(response.SQL, "VACUUM (FREEZE) ")
			if froze != c.freeze {
				t.Fatalf("freeze = %v (%#v), want %v", froze, response, c.freeze)
			}
			if !c.freeze && (response.SQL != "" || response.Kind != "") {
				t.Fatalf("futile freeze still answered %#v", response)
			}
			if (response.PinnedBy != nil) != c.pinned {
				t.Fatalf("pinned = %#v, want %v", response.PinnedBy, c.pinned)
			}
			if c.pinned && response.PinnedBy.PID != 7 {
				t.Fatalf("pinned by %#v, want pid 7", response.PinnedBy)
			}
		})
	}
}

func TestPlanResponseNamesThePinningHolderInFreezeEvidence(t *testing.T) {
	holder := &XminBlocker{Kind: HolderSession, PID: 9, XminAge: 800, AppName: "pg_dump",
		Protected: true, State: "active"}
	response, err := PlanResponse(redInput(1000, holder))
	if err != nil {
		t.Fatalf("PlanResponse: %v", err)
	}
	named, ok := response.Evidence["xmin_holder"].(map[string]any)
	if response.Kind != ResponseVacuumFreeze || !ok {
		t.Fatalf("freeze evidence = %#v", response.Evidence)
	}
	if named["kind"] != HolderSession || named["pid"] != 9 || named["xmin_age"] != int64(800) ||
		named["app_name"] != "pg_dump" || named["protected"] != true {
		t.Fatalf("named holder = %#v", named)
	}
	unpinned, err := PlanResponse(redInput(1000, session(9, 10)))
	if err != nil || unpinned.Evidence["xmin_holder"] != nil || unpinned.PinnedBy != nil {
		t.Fatalf("unpinned freeze = %#v, %v; want no holder", unpinned, err)
	}
}

// Only a red table attaches a holder: an amber one keeps its ladder.
func TestPlanResponseIgnoresHoldersBelowRed(t *testing.T) {
	input := redInput(1000, session(3, 990))
	input.Urgency, input.DeadTupleRatio = UrgencyAmber, 0.3
	response, err := PlanResponse(input)
	// vacuum-07: an amber table gets no custodian response at all (the
	// analyzer owns autovacuum tuning), and still no holder.
	if err != nil || response.Kind != "" || response.SQL != "" || response.PinnedBy != nil {
		t.Fatalf("amber response = %#v, %v", response, err)
	}
}

func TestPlanResponseZeroTableAgeFreezesWithoutPin(t *testing.T) {
	response, err := PlanResponse(redInput(0, session(3, 0)))
	if err != nil || response.Kind != ResponseVacuumFreeze || response.PinnedBy != nil {
		t.Fatalf("zero-age response = %#v, %v", response, err)
	}
}

func TestPinsBoundaries(t *testing.T) {
	for _, c := range []struct {
		holder, table int64
		want          bool
	}{{500, 1000, true}, {499, 1000, false}, {0, 0, false}, {10, 0, false},
		{-1, 1000, false}, {2000, 1000, true}} {
		if got := Pins(c.holder, c.table); got != c.want {
			t.Fatalf("Pins(%d, %d) = %v, want %v", c.holder, c.table, got, c.want)
		}
	}
}

func TestPinningCandidateAppliesSlotCatalogOnlyToCatalogTables(t *testing.T) {
	holders := []XminBlocker{
		{Kind: HolderSlotCatalog, Name: "cdc", XminAge: 900},
		{Kind: HolderPreparedXact, Name: "abc", XminAge: 400},
		{Kind: HolderSession, PID: 5, XminAge: 600},
	}
	user := PinningCandidate(holders, false)
	if user == nil || user.Kind != HolderSession || user.PID != 5 {
		t.Fatalf("user table candidate = %#v, want the oldest applicable session", user)
	}
	catalog := PinningCandidate(holders, true)
	if catalog == nil || catalog.Kind != HolderSlotCatalog || catalog.Name != "cdc" {
		t.Fatalf("catalog table candidate = %#v, want slot cdc", catalog)
	}
	if got := PinningCandidate(nil, false); got != nil {
		t.Fatalf("no holders gave %#v", got)
	}
	only := []XminBlocker{{Kind: HolderSlotCatalog, Name: "cdc", XminAge: 900}}
	if got := PinningCandidate(only, false); got != nil {
		t.Fatalf("catalog_xmin pinned a user table: %#v", got)
	}
}

func TestHolderKeyIdentifiesEachHolder(t *testing.T) {
	start := time.Date(2026, 11, 6, 1, 2, 3, 4000, time.UTC)
	a := XminBlocker{Kind: HolderSession, PID: 5, BackendStart: start}
	b := XminBlocker{Kind: HolderSession, PID: 5, BackendStart: start.Add(time.Second)}
	slot := XminBlocker{Kind: HolderSlot, Name: "s"}
	catalog := XminBlocker{Kind: HolderSlotCatalog, Name: "s"}
	if HolderKey(a) == HolderKey(b) || HolderKey(slot) == HolderKey(catalog) ||
		HolderKey(a) != HolderKey(a) {
		t.Fatalf("keys a=%q b=%q slot=%q catalog=%q", HolderKey(a), HolderKey(b),
			HolderKey(slot), HolderKey(catalog))
	}
}

var pinnedTargets = []string{"public.a", "public.b"}

func TestHolderResponseSignalsOnlyUnprotectedSessions(t *testing.T) {
	start := time.Now().Add(-time.Hour).UTC()
	xid := int64(1234)
	idle := XminBlocker{Kind: HolderSession, PID: 42, XminAge: 1234, User: "app",
		State: "idle in transaction", BackendStart: start, BackendXIDAge: &xid}
	response, err := HolderResponse(idle, pinnedTargets)
	if err != nil {
		t.Fatalf("HolderResponse: %v", err)
	}
	if response.Kind != ResponseTerminateBlocker ||
		response.SQL != "SELECT pg_terminate_backend(42)" || response.Plan != "" {
		t.Fatalf("idle-in-transaction response = %#v", response)
	}
	ev := response.Evidence
	if ev["pid"] != 42 || ev["backend_start"] != start.Format(time.RFC3339Nano) ||
		ev["backend_xid_age"] != int64(1234) || ev["holder_kind"] != HolderSession ||
		ev["pinned_tables"] != 2 || ev["user"] != "app" {
		t.Fatalf("session evidence = %#v", ev)
	}
	active := idle
	active.State = "active"
	if r, err := HolderResponse(active, pinnedTargets); err != nil ||
		r.Kind != ResponseCancelBlocker || r.SQL != "SELECT pg_cancel_backend(42)" {
		t.Fatalf("active response = %#v, %v", r, err)
	}
	protected := active
	protected.Protected, protected.AppName = true, "pg_basebackup"
	r, err := HolderResponse(protected, pinnedTargets)
	if err != nil || r.SQL != "" || r.Kind != ResponseEscalateHolder ||
		!strings.Contains(r.Plan, "42") || !strings.Contains(r.Plan, "pg_basebackup") {
		t.Fatalf("protected session response = %#v, %v", r, err)
	}
}

func TestHolderResponseEscalatesNonSessionHolders(t *testing.T) {
	cases := map[string]struct {
		holder XminBlocker
		names  []string
	}{
		"prepared": {XminBlocker{Kind: HolderPreparedXact, Name: "9f86d081", XminAge: 77},
			[]string{"9f86d081", "prepared transaction"}},
		"slot": {XminBlocker{Kind: HolderSlot, Name: "standby_a", XminAge: 77},
			[]string{`"standby_a"`, "xmin"}},
		"slot catalog": {XminBlocker{Kind: HolderSlotCatalog, Name: "cdc", XminAge: 77},
			[]string{`"cdc"`, "catalog_xmin"}},
		"standby": {XminBlocker{Kind: HolderStandby, PID: 31, XminAge: 77},
			[]string{"31", "hot_standby_feedback"}},
	}
	for name, c := range cases {
		response, err := HolderResponse(c.holder, pinnedTargets)
		if err != nil {
			t.Fatalf("%s: HolderResponse: %v", name, err)
		}
		if response.SQL != "" || response.Kind != ResponseEscalateHolder ||
			response.Plan == "" {
			t.Fatalf("%s: response = %#v", name, response)
		}
		for _, want := range c.names {
			if !strings.Contains(response.Plan, want) {
				t.Fatalf("%s: plan %q does not name %q", name, response.Plan, want)
			}
		}
		upper := strings.ToUpper(response.Plan + response.SQL)
		if strings.Contains(upper, "ROLLBACK PREPARED") ||
			strings.Contains(upper, "PG_DROP_REPLICATION_SLOT") {
			t.Fatalf("%s: escalation carries a resolution statement: %q", name, upper)
		}
		if response.Evidence["holder_kind"] != c.holder.Kind ||
			response.Evidence["holder_name"] != c.holder.Name ||
			response.Evidence["xmin_age"] != int64(77) {
			t.Fatalf("%s: evidence = %#v", name, response.Evidence)
		}
	}
}

func TestHolderResponseRejectsInvalidHolders(t *testing.T) {
	for name, c := range map[string]struct {
		holder  XminBlocker
		targets []string
	}{
		"no targets":       {*session(4, 10), nil},
		"unknown kind":     {XminBlocker{Kind: "lock", PID: 4}, pinnedTargets},
		"empty kind":       {XminBlocker{PID: 4}, pinnedTargets},
		"session pid zero": {XminBlocker{Kind: HolderSession}, pinnedTargets},
		"unnamed slot":     {XminBlocker{Kind: HolderSlot}, pinnedTargets},
		"unnamed prepared": {XminBlocker{Kind: HolderPreparedXact}, pinnedTargets},
	} {
		if response, err := HolderResponse(c.holder, c.targets); err == nil {
			t.Fatalf("%s: accepted, response %#v", name, response)
		}
	}
}
