package freeze

import (
	"slices"
	"strings"
	"testing"
)

// vacuum-05b: a red table whose custodian VACUUM the watchdog capped twice
// is frozen with the emergency profile (no index cleanup, no truncation).
// No concurrent access tests: every function here is pure and keeps no
// shared state.

var knownAges = AggressiveAges{XID: 150_000_000, MultiXact: 150_000_000, Known: true}

func emergencyInput(capFailures int) ResponseInput {
	return ResponseInput{Schema: "public", Table: "orders", Urgency: UrgencyRed,
		XIDAge: 10_000, MultiXactAge: 10, Aggressive: knownAges, CapFailures: capFailures}
}

// Happy path: both forms keep VacuumSQL's plain-or-FREEZE choice.
func TestEmergencyVacuumSQLKeepsThePlainOrFreezeChoice(t *testing.T) {
	plain := EmergencyVacuumSQL("public", "orders", 160_000_000, 0, knownAges)
	if want := `VACUUM (INDEX_CLEANUP OFF, TRUNCATE OFF) "public"."orders"`; plain != want {
		t.Fatalf("aggressive emergency SQL = %q, want %q", plain, want)
	}
	frozen := EmergencyVacuumSQL("public", "orders", 10_000, 0, knownAges)
	want := `VACUUM (FREEZE, INDEX_CLEANUP OFF, TRUNCATE OFF) "public"."orders"`
	if frozen != want {
		t.Fatalf("non-aggressive emergency SQL = %q, want %q", frozen, want)
	}
}

// Nil/empty: unknown aggressive ages keep FREEZE plus the emergency options.
func TestEmergencyVacuumSQLWithUnknownAgesKeepsFreeze(t *testing.T) {
	got := EmergencyVacuumSQL("public", "orders", 160_000_000, 0, AggressiveAges{})
	want := `VACUUM (FREEZE, INDEX_CLEANUP OFF, TRUNCATE OFF) "public"."orders"`
	if got != want {
		t.Fatalf("emergency SQL with unknown ages = %q, want %q", got, want)
	}
}

// Invalid input: identifiers are quoted, so a hostile name cannot add an
// option or a statement.
func TestEmergencyVacuumSQLQuotesIdentifiers(t *testing.T) {
	got := EmergencyVacuumSQL(`s"x`, `t); DROP TABLE y; --`, 0, 0, AggressiveAges{})
	want := `VACUUM (FREEZE, INDEX_CLEANUP OFF, TRUNCATE OFF) "s""x".` +
		`"t); DROP TABLE y; --"`
	if got != want {
		t.Fatalf("quoted emergency SQL = %q, want %q", got, want)
	}
}

// Boundary: 1 cap failure keeps the normal SQL, 2 switch to emergency.
func TestPlanResponseEmergencyStartsAtTheSecondCapFailure(t *testing.T) {
	cases := []struct {
		failures  int
		sql       string
		emergency bool
	}{
		{0, `VACUUM (FREEZE) "public"."orders"`, false},
		{1, `VACUUM (FREEZE) "public"."orders"`, false},
		{2, `VACUUM (FREEZE, INDEX_CLEANUP OFF, TRUNCATE OFF) "public"."orders"`, true},
		{7, `VACUUM (FREEZE, INDEX_CLEANUP OFF, TRUNCATE OFF) "public"."orders"`, true},
	}
	for _, c := range cases {
		response, err := PlanResponse(emergencyInput(c.failures))
		if err != nil {
			t.Fatalf("%d failures: PlanResponse: %v", c.failures, err)
		}
		if response.Kind != ResponseVacuumFreeze || response.SQL != c.sql {
			t.Fatalf("%d failures: response %q (%s), want %q", c.failures, response.SQL,
				response.Kind, c.sql)
		}
		profile, ok := response.Evidence["profile"]
		if c.emergency != ok || (ok && profile != ProfileEmergency) {
			t.Fatalf("%d failures: profile evidence = %#v (present %v)", c.failures,
				profile, ok)
		}
		if c.emergency && response.Evidence["cap_failures"] != c.failures {
			t.Fatalf("%d failures: cap_failures evidence = %#v", c.failures,
				response.Evidence["cap_failures"])
		}
	}
	if EmergencyCapFailures != 2 {
		t.Fatalf("EmergencyCapFailures = %d, want 2", EmergencyCapFailures)
	}
}

// Boundary: an aggressive red table's emergency SQL is plain plus the options.
func TestPlanResponseEmergencyOfAnAggressiveTableIsPlain(t *testing.T) {
	input := emergencyInput(2)
	input.XIDAge = 160_000_000
	response, err := PlanResponse(input)
	if err != nil {
		t.Fatalf("PlanResponse: %v", err)
	}
	if want := `VACUUM (INDEX_CLEANUP OFF, TRUNCATE OFF) "public"."orders"`; response.SQL != want {
		t.Fatalf("emergency SQL = %q, want %q", response.SQL, want)
	}
}

// Boundary: amber is never emergency, whatever its cap failures.
func TestAmberTableWithCapFailuresIsNeverEmergency(t *testing.T) {
	input := emergencyInput(5)
	input.Urgency = UrgencyAmber
	response, err := PlanResponse(input)
	if err != nil {
		t.Fatalf("PlanResponse: %v", err)
	}
	if strings.Contains(response.SQL, "INDEX_CLEANUP") || response.Evidence["profile"] != nil {
		t.Fatalf("amber response = %#v, want no emergency profile", response)
	}
	if got := freezeSQL(input); got != `VACUUM (FREEZE) "public"."orders"` {
		t.Fatalf("amber freeze SQL = %q, want the normal VACUUM (FREEZE)", got)
	}
}

// A red table pinned by a young holder keeps the emergency SQL and the
// holder evidence.
func TestPinnedRedTableKeepsTheEmergencyProfile(t *testing.T) {
	input := emergencyInput(2)
	input.XIDAge = 1_000_000
	// 800k of 1M pins the table, but freezing still helps (below 0.9).
	input.Holder = &XminBlocker{Kind: HolderSession, PID: 42, XminAge: 800_000,
		State: "idle in transaction"}
	if !Pins(input.Holder.XminAge, input.XIDAge) {
		t.Fatal("fixture holder does not pin the table")
	}
	response, err := PlanResponse(input)
	if err != nil {
		t.Fatalf("PlanResponse: %v", err)
	}
	if !strings.Contains(response.SQL, "INDEX_CLEANUP OFF, TRUNCATE OFF") ||
		response.Evidence["profile"] != ProfileEmergency ||
		response.Evidence["xmin_holder"] == nil || response.PinnedBy == nil {
		t.Fatalf("pinned emergency response = %#v", response)
	}
}

// Invalid input: a negative cap failure count is refused.
func TestPlanResponseRefusesNegativeCapFailures(t *testing.T) {
	_, err := PlanResponse(emergencyInput(-1))
	if err == nil || !strings.Contains(err.Error(), "cap failure count -1") {
		t.Fatalf("error = %v, want the negative cap failure count named", err)
	}
}

func TestIsEmergencyVacuumSQLReadsOnlyTheOptionList(t *testing.T) {
	cases := map[string]bool{
		`VACUUM (INDEX_CLEANUP OFF, TRUNCATE OFF) "public"."orders"`:         true,
		`VACUUM (FREEZE, INDEX_CLEANUP OFF, TRUNCATE OFF) "public"."orders"`: true,
		`vacuum (freeze, index_cleanup off) public.orders`:                   true,
		`VACUUM (FREEZE) "public"."orders"`:                                  false,
		`VACUUM "public"."orders"`:                                           false,
		`VACUUM (INDEX_CLEANUP ON) "public"."orders"`:                        false,
		`VACUUM "public"."INDEX_CLEANUP OFF"`:                                false,
		`VACUUM FULL "public"."orders"`:                                      false,
		`ANALYZE (INDEX_CLEANUP OFF) "public"."orders"`:                      false,
		``: false,
	}
	for sql, want := range cases {
		if got := IsEmergencyVacuumSQL(sql); got != want {
			t.Fatalf("IsEmergencyVacuumSQL(%q) = %v, want %v", sql, got, want)
		}
	}
}

// The statements whose cap failures count are every custodian VACUUM of
// the table, normal and emergency.
func TestCapFailuresStatementsListEveryCustodianForm(t *testing.T) {
	got := CustodianVacuumStatements("public", "orders")
	want := []string{`VACUUM "public"."orders"`, `VACUUM (FREEZE) "public"."orders"`,
		`VACUUM (INDEX_CLEANUP OFF, TRUNCATE OFF) "public"."orders"`,
		`VACUUM (FREEZE, INDEX_CLEANUP OFF, TRUNCATE OFF) "public"."orders"`}
	if !slices.Equal(got, want) {
		t.Fatalf("statements = %q, want %q", got, want)
	}
	for _, sql := range got {
		if strings.Contains(strings.ToUpper(sql), "FULL") {
			t.Fatalf("custodian statement %q is a VACUUM FULL", sql)
		}
	}
}

// The executor's watchdog outcome is this package's constant.
func TestCapFailuresOutcomeIsTheWatchdogPrefix(t *testing.T) {
	if VacuumCapOutcome != "vacuum_cap" {
		t.Fatalf("VacuumCapOutcome = %q, want vacuum_cap", VacuumCapOutcome)
	}
}
