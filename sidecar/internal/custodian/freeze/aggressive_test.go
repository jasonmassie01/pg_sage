package freeze

import (
	"strings"
	"testing"
)

// vacuum-05: at or above the effective freeze table age a plain VACUUM is
// already aggressive (it scans every unfrozen page and advances
// relfrozenxid) and writes far less WAL than VACUUM (FREEZE).

// No concurrent access tests: these functions are pure and share no state.

func TestAggressiveVacuumAgesCapsTableAgeAtNinetyFivePercentOfMax(t *testing.T) {
	tests := []struct {
		name               string
		xidTable, xidMax   int64
		mxidTable, mxidMax int64
		wantXID, wantMXID  int64
		wantKnown          bool
	}{
		{name: "defaults keep vacuum_freeze_table_age",
			xidTable: 150_000_000, xidMax: 200_000_000,
			mxidTable: 150_000_000, mxidMax: 400_000_000,
			wantXID: 150_000_000, wantMXID: 150_000_000, wantKnown: true},
		{name: "table age above 95 percent of the max is capped",
			xidTable: 1_000_000_000, xidMax: 200_000_000,
			mxidTable: 2_000_000_000, mxidMax: 400_000_000,
			wantXID: 190_000_000, wantMXID: 380_000_000, wantKnown: true},
		{name: "zero table age means every VACUUM is aggressive",
			xidTable: 0, xidMax: 200_000_000, mxidTable: 0, mxidMax: 400_000_000,
			wantXID: 0, wantMXID: 0, wantKnown: true},
		{name: "unmeasured xid max is unknown",
			xidTable: 150_000_000, xidMax: 0, mxidTable: 150_000_000, mxidMax: 400_000_000},
		{name: "unmeasured multixact max is unknown",
			xidTable: 150_000_000, xidMax: 200_000_000, mxidTable: 150_000_000, mxidMax: -1},
		{name: "negative table age is unknown",
			xidTable: -1, xidMax: 200_000_000, mxidTable: 150_000_000, mxidMax: 400_000_000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := AggressiveVacuumAges(tc.xidTable, tc.xidMax, tc.mxidTable, tc.mxidMax)
			if got.Known != tc.wantKnown {
				t.Fatalf("Known = %v, want %v (%+v)", got.Known, tc.wantKnown, got)
			}
			if tc.wantKnown && (got.XID != tc.wantXID || got.MultiXact != tc.wantMXID) {
				t.Fatalf("ages = %d/%d, want %d/%d", got.XID, got.MultiXact,
					tc.wantXID, tc.wantMXID)
			}
		})
	}
}

func TestVacuumSQLBoundaryAtEffectiveFreezeTableAge(t *testing.T) {
	at := AggressiveVacuumAges(150_000_000, 200_000_000, 150_000_000, 400_000_000)
	const plain = `VACUUM "public"."orders"`
	const freeze = `VACUUM (FREEZE) "public"."orders"`
	tests := []struct {
		name       string
		xid, mxid  int64
		aggressive AggressiveAges
		want       string
	}{
		{"xid age equal to the effective age is plain", 150_000_000, 10, at, plain},
		{"xid age one below is FREEZE", 149_999_999, 10, at, freeze},
		{"xid age above is plain", 1_500_000_000, 10, at, plain},
		{"multixact age equal to its effective age is plain", 10, 150_000_000, at, plain},
		{"multixact age one below is FREEZE", 10, 149_999_999, at, freeze},
		{"unknown ages always FREEZE", 1_500_000_000, 1_500_000_000, AggressiveAges{}, freeze},
		{"zero ages with unknown flag FREEZE", 0, 0, AggressiveAges{}, freeze},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := VacuumSQL("public", "orders", tc.xid, tc.mxid, tc.aggressive)
			if got != tc.want {
				t.Fatalf("VacuumSQL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVacuumSQLQuotesIdentifiers(t *testing.T) {
	at := AggressiveVacuumAges(10, 200, 10, 200)
	got := VacuumSQL(`we"ird`, `t"x`, 100, 0, at)
	if got != `VACUUM "we""ird"."t""x"` {
		t.Fatalf("VacuumSQL = %q", got)
	}
}

// CHECK-04: the freeze custodian SQL of a red table whose age is above the
// effective freeze table age contains no FREEZE.
func TestPlanResponsePlainVacuumAboveFreezeTableAge(t *testing.T) {
	at := AggressiveVacuumAges(150_000_000, 200_000_000, 150_000_000, 400_000_000)
	for _, tc := range []struct {
		age  int64
		want string
	}{
		{age: 160_000_000, want: `VACUUM "public"."orders"`},
		{age: 150_000_000, want: `VACUUM "public"."orders"`},
		{age: 149_999_999, want: `VACUUM (FREEZE) "public"."orders"`},
	} {
		response, err := PlanResponse(ResponseInput{Schema: "public", Table: "orders",
			Urgency: UrgencyRed, XIDAge: tc.age, MultiXactAge: 5, Aggressive: at})
		if err != nil {
			t.Fatalf("age %d: PlanResponse: %v", tc.age, err)
		}
		if response.Kind != ResponseVacuumFreeze || response.SQL != tc.want {
			t.Fatalf("age %d: response = %q %q, want %q", tc.age, response.Kind,
				response.SQL, tc.want)
		}
		if tc.age >= 150_000_000 && strings.Contains(response.SQL, "FREEZE") {
			t.Fatalf("age %d: SQL %q names FREEZE above the freeze table age",
				tc.age, response.SQL)
		}
	}
}

func TestPlanResponseRejectsNegativeMultiXactAge(t *testing.T) {
	_, err := PlanResponse(ResponseInput{Schema: "public", Table: "orders",
		Urgency: UrgencyRed, XIDAge: 10, MultiXactAge: -1})
	if err == nil || !strings.Contains(err.Error(), "multixact age") {
		t.Fatalf("PlanResponse error = %v, want a multixact age error", err)
	}
}
