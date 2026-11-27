package srebench

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/gameday"
	"github.com/pg-sage/sidecar/internal/sre"
)

// benches-12: game days exercise the customer's database. The customer
// program lock_customer_hot_table locks the table its binding names (the
// monitored database's hottest table, chosen by internal/gameday) on the
// clone, reads and writes no row, and leaves the table as it found it.
//
// No concurrent access tests here: a program's state is built per call of
// customerScenarios; the harness runs scenarios one at a time.

func ordersBinding() gameday.Binding {
	return gameday.Binding{Program: gameday.ProgramCustomerHotTable, Schema: "public",
		Table: "orders_secret"}
}

func TestCustomerScenariosFromBindings(t *testing.T) {
	none, err := customerScenarios(nil)
	if err != nil || len(none) != 0 {
		t.Fatalf("no bindings = %d scenarios (%v), want none", len(none), err)
	}
	got, err := customerScenarios([]gameday.Binding{ordersBinding()})
	if err != nil || len(got) != 1 {
		t.Fatalf("one binding = %d scenarios (%v)", len(got), err)
	}
	sc := got[0]
	if sc.ID != gameday.ProgramCustomerHotTable || sc.Family != sre.TriggerLock ||
		sc.Class != ClassPositive || sc.Gold.Root != "ddl_lock_queue" || sc.Program == nil {
		t.Fatalf("scenario = %+v", sc)
	}
	if strings.Contains(sc.Subject, "orders_secret") {
		t.Fatalf("subject %q carries the table name", sc.Subject)
	}
	if _, err := customerScenarios([]gameday.Binding{{Program: "drop_everything",
		Schema: "public", Table: "t"}}); err == nil ||
		!strings.Contains(err.Error(), "drop_everything") {
		t.Fatalf("an unknown customer program = %v, want an error naming it", err)
	}
	if _, err := customerScenarios([]gameday.Binding{ordersBinding(),
		ordersBinding()}); err == nil {
		t.Fatal("two bindings of one program were accepted")
	}
}

// Nil/empty: a skip binding becomes a skipped run with the binding's
// reason, before any SQL runs (the Env has no pool).
func TestCustomerSkipBindingSkipsWithItsReason(t *testing.T) {
	b := gameday.Binding{Program: gameday.ProgramCustomerHotTable,
		Skip: gameday.SkipLocalProvider}
	got, err := customerScenarios([]gameday.Binding{b})
	if err != nil || len(got) != 1 {
		t.Fatalf("skip binding = %d scenarios (%v)", len(got), err)
	}
	err = got[0].Program.Inject(context.Background(), &Env{})
	var u *Unsupported
	if !errors.As(err, &u) || u.Reason != gameday.SkipLocalProvider {
		t.Fatalf("inject = %v, want Unsupported %q", err, gameday.SkipLocalProvider)
	}
	// A binding without a skip reason and without a table is no binding.
	got, err = customerScenarios([]gameday.Binding{{Program: gameday.ProgramCustomerHotTable}})
	if err != nil || len(got) != 1 {
		t.Fatalf("empty binding = %d scenarios (%v)", len(got), err)
	}
	if err := got[0].Program.Inject(context.Background(), &Env{}); !errors.As(err, &u) ||
		u.Reason == "" {
		t.Fatalf("empty binding inject = %v, want a skip with a reason", err)
	}
}

func TestSelectGameDayScenariosAddsTheCustomerProgram(t *testing.T) {
	opts := GameDayOptions{Families: []string{"lock_blocking"},
		Bindings: []gameday.Binding{ordersBinding()}}
	lock, err := selectGameDayScenarios(opts)
	if err != nil {
		t.Fatal(err)
	}
	if lock[len(lock)-1].ID != gameday.ProgramCustomerHotTable {
		t.Fatalf("the customer program is not last of %d lock scenarios", len(lock))
	}
	opts.Families = []string{"wal_retention"}
	wal, err := selectGameDayScenarios(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, sc := range wal {
		if sc.ID == gameday.ProgramCustomerHotTable {
			t.Fatal("a wal_retention game day ran the lock customer program")
		}
	}
	opts.Families, opts.Scenarios = nil, []string{gameday.ProgramCustomerHotTable}
	one, err := selectGameDayScenarios(opts)
	if err != nil || len(one) != 1 || one[0].ID != gameday.ProgramCustomerHotTable {
		t.Fatalf("scenario filter = %d (%v)", len(one), err)
	}
	opts.Bindings = nil
	if _, err := selectGameDayScenarios(opts); err == nil {
		t.Fatal("the customer program was selectable without a binding")
	}
}

// CHECK-03: every LOCK TABLE of the customer program uses the binding's
// quoted identifier, never a formatted or literal name.
func TestCustomerProgramLocksOnlyTheQuotedIdent(t *testing.T) {
	src, err := os.ReadFile("scenarios_customer.go")
	if err != nil {
		t.Fatal(err)
	}
	lines := regexp.MustCompile(`(?m)^.*LOCK TABLE.*$`).FindAll(src, -1)
	if len(lines) < 2 {
		t.Fatalf("%d LOCK TABLE lines, want the holder's and the waiters'", len(lines))
	}
	for _, l := range lines {
		s := string(l)
		if strings.HasPrefix(strings.TrimSpace(s), "//") {
			continue
		}
		if !strings.Contains(s, `"LOCK TABLE " + ident + " IN `) || strings.Contains(s, "%") {
			t.Errorf("LOCK TABLE not built from the quoted ident: %s", s)
		}
	}
	sql := customerHolderSQL(ordersBinding().Ident())
	if !strings.Contains(sql, `LOCK TABLE "public"."orders_secret" IN ROW EXCLUSIVE MODE`) {
		t.Fatalf("holder SQL = %s", sql)
	}
	hostile := gameday.Binding{Program: gameday.ProgramCustomerHotTable, Schema: "public",
		Table: `x"; DROP TABLE y; --`}
	sql = customerWaiterSQL(hostile.Ident(), "SHARE ROW EXCLUSIVE")
	if !strings.Contains(sql, `LOCK TABLE "public"."x""; DROP TABLE y; --" IN SHARE ROW`) ||
		!strings.Contains(sql, "lock_timeout = '90s'") || !strings.HasSuffix(sql, "ROLLBACK") {
		t.Fatalf("waiter SQL = %s", sql)
	}
}

// CHECK-04: the exported report of a customer run names the program and
// never the table: not in a skip, an error, or anywhere else.
func TestCustomerReportCarriesNoTableName(t *testing.T) {
	scs, err := customerScenarios([]gameday.Binding{ordersBinding()})
	if err != nil {
		t.Fatal(err)
	}
	results := []Result{
		{Scenario: scs[0], Arm: ArmCausalGraph, Repeat: 1, Attempts: 1,
			Outcome: Outcome{State: sre.StateConcluded, Root: "ddl_lock_queue"}},
		{Scenario: scs[0], Arm: ArmCausalGraph, Repeat: 2, Attempts: 1,
			Skipped: skipNotInClone},
		{Scenario: scs[0], Arm: ArmCausalGraph, Repeat: 3, Attempts: 1,
			Err: holderError(ordersBinding(), errors.New(
				`ERROR: permission denied for table orders_secret (SQLSTATE 42501)`))},
	}
	raw, err := json.Marshal(BuildReport(results, ReportMeta{Arms: []string{ArmCausalGraph}}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "orders_secret") {
		t.Fatalf("the report carries the table name:\n%s", raw)
	}
	if !strings.Contains(string(raw), gameday.ProgramCustomerHotTable) ||
		!strings.Contains(string(raw), ordersBinding().Hash()) {
		t.Fatalf("the report lacks the program or the binding hash:\n%s", raw)
	}
}
