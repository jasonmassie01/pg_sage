package schema

import (
	"bufio"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// config-04b: the migration ledger against fake sessions. The real-server
// behaviour (seeding, a second boot with no DDL, a changed checksum, an
// INVALID index) is in migration_ledger_db_test.go.
// No concurrent-access tests here: one bootstrap session runs the steps in
// order under the advisory lock (the parallel-boot ledger test is there).

// ledgerExecDB records every Exec with its arguments and fails on demand.
type ledgerExecDB struct {
	fakeStepDB
	args    [][]any
	failSQL string // an Exec whose SQL contains it fails
}

func (f *ledgerExecDB) Exec(
	ctx context.Context, sql string, args ...any,
) (pgconn.CommandTag, error) {
	f.args = append(f.args, args)
	if f.failSQL != "" && strings.Contains(sql, f.failSQL) {
		f.execs = append(f.execs, sql)
		return pgconn.CommandTag{}, errors.New("permission denied for table schema_migrations")
	}
	return f.fakeStepDB.Exec(ctx, sql, args...)
}

func (f *ledgerExecDB) Begin(context.Context) (pgx.Tx, error) {
	f.begins++
	return ledgerTx{fakeTx: fakeTx{db: &f.fakeStepDB}, db: f}, nil
}

type ledgerTx struct {
	fakeTx
	db *ledgerExecDB
}

func (t ledgerTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.db.Exec(ctx, sql, args...)
}

// loadedLedger is a ledger whose catalog fingerprint matched (trusted).
func loadedLedger(recorded map[string]string) *migrationLedger {
	return &migrationLedger{loaded: true, trusted: true, recorded: recorded}
}

// Happy path: the checksum is the SHA-256 hex digest of the step's SQL.
func TestStepChecksum_IsTheSHA256OfTheSQL(t *testing.T) {
	cases := map[string]string{
		"":    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"abc": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
	}
	for sql, want := range cases {
		if got := stepChecksum(sql); got != want {
			t.Errorf("stepChecksum(%q) = %s, want %s", sql, got, want)
		}
	}
	if stepChecksum(testColumnStep) == stepChecksum(testColumnStep+" ") {
		t.Fatal("a one-byte change keeps the checksum")
	}
}

// State transitions: absent runs, recorded skips, changed fails loudly.
func TestMigrationLedger_Decide(t *testing.T) {
	step := migrationStep{name: "ddlX", sql: testColumnStep}
	l := loadedLedger(map[string]string{"ddlX": stepChecksum(testColumnStep)})
	if skip, err := l.decide(step); err != nil || !skip {
		t.Fatalf("recorded step: skip=%v err=%v, want skip", skip, err)
	}
	if skip, err := l.decide(migrationStep{name: "ddlY", sql: "x"}); err != nil || skip {
		t.Fatalf("unrecorded step: skip=%v err=%v, want run", skip, err)
	}
	l.recorded["ddlX"] = "0123abcd"
	_, err := l.decide(step)
	if !errors.Is(err, errMigrationChanged) {
		t.Fatalf("changed step: err = %v, want errMigrationChanged", err)
	}
	for _, want := range []string{"ddlX", "0123abcd", stepChecksum(testColumnStep),
		"DELETE FROM sage.schema_migrations WHERE id = 'ddlX'"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// Nil/empty: no ledger (a caller outside Bootstrap) never skips; an empty
// ledger runs everything.
func TestMigrationLedger_NilAndEmptyRunEveryStep(t *testing.T) {
	var none *migrationLedger
	step := migrationStep{name: "ddlX", sql: testColumnStep}
	if skip, err := none.decide(step); err != nil || skip {
		t.Fatalf("nil ledger: skip=%v err=%v", skip, err)
	}
	if skip, err := loadedLedger(nil).decide(step); err != nil || skip {
		t.Fatalf("empty ledger: skip=%v err=%v", skip, err)
	}
	if ledgerFrom(context.Background()) != nil {
		t.Fatal("a plain context carries a ledger")
	}
}

// Happy path: on a trusted catalog a recorded step runs no SQL at all,
// not even its catalog precheck.
func TestRunMigrationStep_RecordedStepRunsNothing(t *testing.T) {
	db := &ledgerExecDB{}
	const doStep = "DO $$ BEGIN PERFORM 1; END $$"
	l := loadedLedger(map[string]string{
		"ddlX": stepChecksum(testColumnStep), "ddlDo": stepChecksum(doStep)})
	ctx := withMigrationLedger(context.Background(), l)
	for _, step := range []migrationStep{{name: "ddlX", sql: testColumnStep},
		{name: "ddlDo", sql: doStep}} {
		outcome, err := runMigrationStep(ctx, db, step)
		if err != nil || outcome != stepLedgered {
			t.Fatalf("%s: outcome=%v err=%v, want stepLedgered", step.name, outcome, err)
		}
	}
	if db.queries+len(db.execs)+db.begins != 0 || l.ran {
		t.Fatalf("recorded steps touched the session: %d queries, execs %q, %d begins",
			db.queries, db.execs, db.begins)
	}
}

// State transition: on a catalog that changed since the last boot (drift,
// a downgrade, a cut-off boot) a recorded step runs again through its
// catalog checks, as before the ledger, and is not recorded twice.
func TestRunMigrationStep_UntrustedCatalogRerunsARecordedStep(t *testing.T) {
	db := &ledgerExecDB{fakeStepDB: fakeStepDB{answers: [][]bool{{false}}}}
	l := loadedLedger(map[string]string{"ddlX": stepChecksum(testColumnStep)})
	l.trusted = false
	ctx := withMigrationLedger(context.Background(), l)
	outcome, err := runMigrationStep(ctx, db, migrationStep{name: "ddlX", sql: testColumnStep})
	if err != nil || outcome != stepApplied {
		t.Fatalf("outcome=%v err=%v, want stepApplied", outcome, err)
	}
	if db.commits != 1 || len(db.execs) != 1 || !strings.Contains(db.execs[0], testColumnStep) {
		t.Fatalf("the step did not run once: execs %q, %d commits", db.execs, db.commits)
	}
	if !l.ran {
		t.Fatal("a step ran but the ledger does not know the catalog may have changed")
	}
}

// Error propagation: a changed step fails loudly on an untrusted catalog
// too.
func TestMigrationLedger_ChangedStepFailsUntrusted(t *testing.T) {
	l := loadedLedger(map[string]string{"ddlX": "feedface"})
	l.trusted = false
	if _, err := l.decide(migrationStep{name: "ddlX", sql: testColumnStep}); !errors.Is(err,
		errMigrationChanged) {
		t.Fatalf("err = %v, want errMigrationChanged", err)
	}
}

// State transitions of seal: a boot that ran nothing on a trusted catalog
// writes nothing; any other loaded boot stores the fingerprint; a ledger
// never loaded (the boot failed before its first step) writes nothing.
func TestMigrationLedger_Seal(t *testing.T) {
	SetBuildVersion("2.6.0-test")
	t.Cleanup(func() { SetBuildVersion("") })
	cases := []struct {
		name                 string
		loaded, trusted, ran bool
		writes               bool
	}{
		{"trusted, nothing ran", true, true, false, false},
		{"trusted, a step ran", true, true, true, true},
		{"untrusted", true, false, false, true},
		{"never loaded", false, false, true, false},
	}
	for _, c := range cases {
		db := &ledgerExecDB{}
		l := &migrationLedger{loaded: c.loaded, trusted: c.trusted, ran: c.ran}
		if err := l.seal(context.Background(), db); err != nil {
			t.Fatalf("%s: seal: %v", c.name, err)
		}
		if got := len(db.execs) == 1; got != c.writes || len(db.execs) > 1 {
			t.Fatalf("%s: execs %q, want a write %v", c.name, db.execs, c.writes)
		}
		if c.writes && (db.args[0][0] != ledgerCatalogID || db.args[0][1] != "2.6.0-test") {
			t.Fatalf("%s: seal args %v", c.name, db.args[0])
		}
	}
	var none *migrationLedger
	if err := none.seal(context.Background(), &ledgerExecDB{}); err != nil {
		t.Fatalf("nil ledger: %v", err)
	}
}

// Error propagation: a changed step fails before any SQL, naming the step.
func TestRunMigrationStep_ChangedStepFailsBeforeAnySQL(t *testing.T) {
	db := &ledgerExecDB{}
	ctx := withMigrationLedger(context.Background(),
		loadedLedger(map[string]string{"ddlX": "feedface"}))
	_, err := runMigrationStep(ctx, db, migrationStep{name: "ddlX", sql: testColumnStep})
	if !errors.Is(err, errMigrationChanged) || !strings.Contains(err.Error(), "ddlX") {
		t.Fatalf("err = %v, want errMigrationChanged naming ddlX", err)
	}
	if db.queries+len(db.execs)+db.begins != 0 {
		t.Fatalf("changed step touched the session: execs %q", db.execs)
	}
}

// Seeding: a step the catalog shows done is recorded with its checksum and
// the build version, and only then counts as skipped.
func TestRunMigrationStep_RecordsAStepTheCatalogShowsDone(t *testing.T) {
	SetBuildVersion("2.6.0-test")
	t.Cleanup(func() { SetBuildVersion("") })
	db := &ledgerExecDB{fakeStepDB: fakeStepDB{answers: [][]bool{{true}}}}
	l := loadedLedger(map[string]string{})
	ctx := withMigrationLedger(context.Background(), l)
	outcome, err := runMigrationStep(ctx, db, migrationStep{name: "ddlX", sql: testColumnStep})
	if err != nil || outcome != stepSkipped {
		t.Fatalf("outcome=%v err=%v, want stepSkipped", outcome, err)
	}
	if len(db.execs) != 1 || !strings.Contains(db.execs[0], "INSERT INTO sage.schema_migrations") {
		t.Fatalf("execs = %q, want one ledger insert", db.execs)
	}
	want := []any{"ddlX", stepChecksum(testColumnStep), "2.6.0-test"}
	if got := db.args[0]; len(got) != 3 || got[0] != want[0] || got[1] != want[1] ||
		got[2] != want[2] {
		t.Fatalf("ledger insert args = %v, want %v", got, want)
	}
	if l.recorded["ddlX"] != stepChecksum(testColumnStep) || !l.ran {
		t.Fatalf("in-memory ledger not updated: %v ran=%v", l.recorded, l.ran)
	}
}

// Error propagation: a ledger write that fails fails the step, naming it,
// so the boot never reports a step done that the ledger does not hold.
func TestRunMigrationStep_LedgerWriteFailureFailsTheStep(t *testing.T) {
	db := &ledgerExecDB{fakeStepDB: fakeStepDB{answers: [][]bool{{false}}},
		failSQL: "INSERT INTO sage.schema_migrations"}
	ctx := withMigrationLedger(context.Background(), loadedLedger(map[string]string{}))
	_, err := runMigrationStep(ctx, db, migrationStep{name: "ddlX", sql: testColumnStep})
	if err == nil || !strings.Contains(err.Error(), "ddlX") ||
		!strings.Contains(err.Error(), "schema_migrations") {
		t.Fatalf("err = %v, want a ledger error naming ddlX", err)
	}
}

// Invalid input: an index statement the catalog does not show valid first
// drops an INVALID index of that name, then builds it.
func TestApplyStep_DropsAnInvalidIndexBeforeBuildingIt(t *testing.T) {
	db := &fakeStepDB{answers: [][]bool{{false}}}
	stmts := classifyStep("CREATE INDEX IF NOT EXISTS idx_x ON sage.t (a)")
	if _, err := applyStep(context.Background(), db, stmts); err != nil {
		t.Fatalf("applyStep: %v", err)
	}
	if len(db.execs) != 1 {
		t.Fatalf("execs = %q, want one batch", db.execs)
	}
	batch := db.execs[0]
	drop := strings.Index(batch, "DROP INDEX sage.idx_x")
	build := strings.Index(batch, "CREATE INDEX IF NOT EXISTS idx_x")
	if drop < 0 || build < 0 || drop > build {
		t.Fatalf("batch does not drop the invalid index before the build:\n%s", batch)
	}
	if !strings.Contains(batch, "NOT indisvalid") {
		t.Fatalf("the drop is not guarded by indisvalid:\n%s", batch)
	}
}

// The report names how many steps the ledger skipped, only when some did.
func TestMigrationReport_CountsLedgeredSteps(t *testing.T) {
	var r migrationReport
	r.steps = 2
	r.count("a", stepLedgered, 0)
	r.count("b", stepLedgered, 0)
	got := r.String()
	if !strings.Contains(got, "0 skipped") || !strings.Contains(got, "2 recorded") {
		t.Fatalf("String() = %q, want 2 recorded", got)
	}
	if r.slowest != "" {
		t.Fatalf("a ledgered step competes for slowest: %q", r.slowest)
	}
}

func TestBuildVersion_DefaultsToDev(t *testing.T) {
	SetBuildVersion("")
	if got := buildVersion(); got != "dev" {
		t.Fatalf("buildVersion() = %q, want dev", got)
	}
	SetBuildVersion("2.6.0")
	t.Cleanup(func() { SetBuildVersion("") })
	if got := buildVersion(); got != "2.6.0" {
		t.Fatalf("buildVersion() = %q, want 2.6.0", got)
	}
}

// goldenChecksums reads testdata/migration_checksums.txt: "id checksum".
func goldenChecksums(t *testing.T) (map[string]string, []string) {
	t.Helper()
	f, err := os.Open("testdata/migration_checksums.txt")
	if err != nil {
		t.Fatalf("open golden checksums: %v", err)
	}
	defer f.Close()
	sums := map[string]string{}
	var order []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id, sum, ok := strings.Cut(line, " ")
		if !ok || len(sum) != 64 {
			t.Fatalf("golden line %q is not 'id sha256'", line)
		}
		if _, dup := sums[id]; dup {
			t.Fatalf("golden id %s listed twice", id)
		}
		sums[id] = sum
		order = append(order, id)
	}
	return sums, order
}

// A released step never changes: the ledger of every install would fail
// its next boot. Every step is pinned; a new one is added to the file.
func TestMigrationSteps_MatchTheGoldenChecksums(t *testing.T) {
	golden, _ := goldenChecksums(t)
	seen := map[string]bool{}
	for _, s := range migrationSteps() {
		if seen[s.name] {
			t.Fatalf("step name %s is used twice: ledger ids must be unique", s.name)
		}
		seen[s.name] = true
		want, ok := golden[s.name]
		switch {
		case !ok:
			t.Errorf("new step %s: add this line to testdata/migration_checksums.txt:\n%s %s",
				s.name, s.name, stepChecksum(s.sql))
		case want != stepChecksum(s.sql):
			t.Errorf("step %s changed (pinned %s, now %s): a released step must not "+
				"change, add a new step instead", s.name, want, stepChecksum(s.sql))
		}
	}
}

// The ledger table is a tranche-two step; load creates it before any step
// runs, so its place in the tranche does not matter.
func TestMigrationSteps_LedgerTableIsATrancheTwoStep(t *testing.T) {
	if !isTrancheTwoStep("ddlSchemaMigrations") {
		t.Fatalf("trancheTwoMigrations = %v, want ddlSchemaMigrations among them",
			trancheTwoMigrations())
	}
	stmts := classifyStep(ddlSchemaMigrations)
	if len(stmts) != 1 || stmts[0].shape != shapeRelation ||
		stmts[0].checks[0].rel != "sage.schema_migrations" {
		t.Fatalf("ddlSchemaMigrations is not one prechecked CREATE TABLE: %+v", stmts)
	}
}
