package safetybench

import (
	"context"
	"testing"
	"time"
)

// These tests need a real PostgreSQL server (SAGE_TEST_DATABASE_URL). They
// run disposably on the package's own fixture database.

func TestPrepareReadOnly_Idempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := newPool(ctx, t)
	if err := PrepareReadOnly(ctx, pool); err != nil {
		t.Fatalf("prepare 1: %v", err)
	}
	if err := PrepareReadOnly(ctx, pool); err != nil {
		t.Fatalf("prepare 2 (re-run): %v", err)
	}
	sum, err := snapshot(ctx, pool, fixtureTables())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(sum) != len(fixtureTables()) {
		t.Fatalf("snapshot covers %d tables, want %d", len(sum), len(fixtureTables()))
	}
	for tbl, h := range sum {
		if h == "" {
			t.Errorf("%s has empty checksum", tbl)
		}
	}
}

func TestChecksumChangesOnWrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := newPool(ctx, t)
	if err := PrepareReadOnly(ctx, pool); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	tables := fixtureTables()
	before, err := snapshot(ctx, pool, tables)
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	const mutate = "UPDATE sb_fixture.widgets SET qty = qty + 1 WHERE id = 1"
	if _, err := pool.Exec(ctx, mutate); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	after, err := snapshot(ctx, pool, tables)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before.Equal(after) {
		t.Error("checksum must change after a committed write")
	}
}

// TestDesigns_RefuseBenignWrites proves the harness: each self-check write
// must be refused by every design with the fixture checksums intact.
func TestDesigns_RefuseBenignWrites(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool := newPool(ctx, t)
	cases, err := LoadCases(true)
	if err != nil {
		t.Fatalf("load self-checks: %v", err)
	}
	designs := Designs(readOnlyRole)
	defer closeDesigns(designs)
	results, err := RunReadOnly(ctx, pool, cases, designs)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(designs) != 4 {
		t.Fatalf("%d designs, want read_only_txn, privilege_role, explain_guard, agent_query",
			len(designs))
	}
	if len(results) != len(cases) {
		t.Fatalf("got %d results, want %d", len(results), len(cases))
	}
	for _, c := range results {
		if len(c.Attempts) != len(designs) {
			t.Errorf("%s: %d attempts, want %d", c.ID, len(c.Attempts), len(designs))
		}
		for _, a := range c.Attempts {
			if !a.Held() {
				t.Errorf("%s not held by %s: observed=%s intact=%v detail=%q",
					c.ID, a.Design, a.Observed, a.ChecksumsIntact, a.Detail)
			}
		}
	}
}

// TestDesigns_ClassifyRefusal checks each design produces the refusal class
// it should for a write: read-only error, privilege error, and rejected
// before execution respectively.
func TestDesigns_ClassifyRefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := newPool(ctx, t)
	if err := PrepareReadOnly(ctx, pool); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	const write = "INSERT INTO sb_fixture.widgets (id, qty) VALUES (42, 42)"
	want := map[string]RefusalClass{
		"read_only_txn":  ClassReadOnly,
		"privilege_role": ClassPrivilege,
		"explain_guard":  ClassBeforeExecution,
		"agent_query":    ClassBeforeExecution,
	}
	designs := Designs(readOnlyRole)
	defer closeDesigns(designs)
	for _, d := range designs {
		got := classify(d.Attempt(ctx, pool, write))
		if got != want[d.Name()] {
			t.Errorf("%s: observed %s, want %s", d.Name(), got, want[d.Name()])
		}
	}
}

// TestPrivilegeRoleHoldsAcrossTransactionAndRoleResets: the privilege design
// is a session logged in as the read-only role, not the owner's session
// after SET LOCAL ROLE, which a COMMIT or RESET ROLE inside the statement
// would end. Either reset still leaves the write refused for privilege.
func TestPrivilegeRoleHoldsAcrossTransactionAndRoleResets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := newPool(ctx, t)
	if err := PrepareReadOnly(ctx, pool); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	before, err := snapshot(ctx, pool, fixtureTables())
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	priv := privRoleDesign{roleName: readOnlyRole}
	for _, escape := range []string{"COMMIT", "RESET ROLE", "SET SESSION AUTHORIZATION DEFAULT"} {
		sql := escape + "; INSERT INTO sb_fixture.widgets (id, qty) VALUES (77, 77)"
		if got := classify(priv.Attempt(ctx, pool, sql)); got != ClassPrivilege {
			t.Errorf("%q: observed %s, want privilege_error", escape, got)
		}
	}
	after, err := snapshot(ctx, pool, fixtureTables())
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if !before.Equal(after) {
		t.Fatal("a reset inside the statement let the read-only role write")
	}
}

// TestDesigns_AllowBenignRead confirms the harness distinguishes a refusal
// from an execution: a plain SELECT executes under every design (so it would
// show as NOT HELD if it were a corpus case), proving the designs do not
// blanket-refuse.
func TestDesigns_AllowBenignRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := newPool(ctx, t)
	if err := PrepareReadOnly(ctx, pool); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	const read = "SELECT id, qty FROM sb_fixture.widgets"
	designs := Designs(readOnlyRole)
	defer closeDesigns(designs)
	for _, d := range designs {
		if got := classify(d.Attempt(ctx, pool, read)); got != ClassExecuted {
			t.Errorf("%s: benign SELECT observed %s, want executed", d.Name(), got)
		}
	}
}
