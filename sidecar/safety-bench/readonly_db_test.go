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
	results, err := RunReadOnly(ctx, pool, cases, Designs(readOnlyRole))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(results) != len(cases) {
		t.Fatalf("got %d results, want %d", len(results), len(cases))
	}
	for _, c := range results {
		if len(c.Attempts) != 3 {
			t.Errorf("%s: %d attempts, want 3", c.ID, len(c.Attempts))
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
	}
	for _, d := range Designs(readOnlyRole) {
		got := classify(d.Attempt(ctx, pool, write))
		if got != want[d.Name()] {
			t.Errorf("%s: observed %s, want %s", d.Name(), got, want[d.Name()])
		}
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
	for _, d := range Designs(readOnlyRole) {
		if got := classify(d.Attempt(ctx, pool, read)); got != ClassExecuted {
			t.Errorf("%s: benign SELECT observed %s, want executed", d.Name(), got)
		}
	}
}
