package safetybench

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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
// with the role switched for the transaction, which a COMMIT or RESET ROLE
// inside the statement would end. Either reset still leaves the write refused for privilege.
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

// TestSelfCheckFailsWhenDesignCannotLogIn: a design that never reaches the
// server measured nothing. Its self-checks must fail by name, not "hold"
// because the attempt errored with the checksums untouched.
func TestSelfCheckFailsWhenDesignCannotLogIn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool := newPool(ctx, t)
	cases, err := LoadCases(true)
	if err != nil {
		t.Fatalf("load self-checks: %v", err)
	}
	t.Run("missing role", func(t *testing.T) {
		assertOnlyDesignFails(ctx, t, pool, cases, privRoleDesign{roleName: "sb_no_such_role"})
	})
	t.Run("connect revoked", func(t *testing.T) {
		db := currentDB(ctx, t, pool)
		d := &connectRevokedDesign{privRoleDesign: privRoleDesign{roleName: readOnlyRole},
			db: db}
		t.Cleanup(func() { restoreConnect(t, pool, db) })
		assertOnlyDesignFails(ctx, t, pool, cases, d)
		if !d.revoked {
			t.Fatal("the design never revoked CONNECT; the case was not exercised")
		}
	})
}

// assertOnlyDesignFails runs the self-checks against read_only_txn (a
// healthy control) and broken, and requires SelfCheckFailures to name
// broken on every self-check case and never the control.
func assertOnlyDesignFails(ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	cases []Case, broken RODesign) {
	t.Helper()
	designs := []RODesign{readOnlyTxnDesign{}, broken}
	results, err := RunReadOnly(ctx, pool, cases, designs)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, c := range results {
		if got := c.Attempts[1].Observed; got != ClassOtherError {
			t.Errorf("%s: broken design observed %s, want other_error (detail %q)",
				c.ID, got, c.Attempts[1].Detail)
		}
	}
	r := BuildReport(ReportMeta{Designs: designNames(designs)}, results, nil, nil)
	failures := SelfCheckFailures(r)
	if len(failures) != len(cases) {
		t.Fatalf("want %d self-check failures (one per case), got %q", len(cases), failures)
	}
	for _, f := range failures {
		if !strings.Contains(f, "not held by "+broken.Name()) {
			t.Errorf("failure does not name %s: %q", broken.Name(), f)
		}
		if strings.Contains(f, "read_only_txn") {
			t.Errorf("failure blames the healthy control: %q", f)
		}
	}
	if s := r.ReadOnlySummary()[broken.Name()]; s.Held != 0 || s.NotMeasured != len(cases) {
		t.Errorf("broken design summary = %+v, want Held=0 NotMeasured=%d", s, len(cases))
	}
}

// connectRevokedDesign is privRoleDesign whose role loses CONNECT on the
// fixture database after PrepareReadOnly granted it (on its first attempt).
// The login then fails with 42501, which must not read as a privilege
// refusal of the statement.
type connectRevokedDesign struct {
	privRoleDesign
	db      string
	revoked bool
}

func (d *connectRevokedDesign) Attempt(ctx context.Context, owner *pgxpool.Pool,
	sql string) error {
	if !d.revoked {
		for _, grantee := range []string{"PUBLIC", quoteIdent(d.roleName)} {
			stmt := "REVOKE CONNECT ON DATABASE " + quoteIdent(d.db) + " FROM " + grantee
			if _, err := owner.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("test setup %q: %w", stmt, err)
			}
		}
		d.revoked = true
	}
	return d.privRoleDesign.Attempt(ctx, owner, sql)
}

func currentDB(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var db string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatalf("read the fixture database name: %v", err)
	}
	return db
}

// restoreConnect gives PUBLIC its default CONNECT back; the next
// PrepareReadOnly re-grants the fixture roles.
func restoreConnect(t *testing.T, pool *pgxpool.Pool, db string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stmt := "GRANT CONNECT ON DATABASE " + quoteIdent(db) + " TO PUBLIC"
	if _, err := pool.Exec(ctx, stmt); err != nil {
		t.Errorf("restore CONNECT: %v", err)
	}
}
