package gitops

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/policy"
)

// cli-02a on a real server (PG14 and PG17): a directory reconciles into
// rows marked config through the stores, in one transaction.

func TestReconcile_HappyPathThenNoOp(t *testing.T) {
	f := newFixture(t)
	root := writeTree(t, f.happyFiles())
	res := f.apply(t, root)
	want := map[string]int{"policy_set:create": 2, "policy_set:ratify": 1,
		"principal:create": 1, "grant:create": 2}
	if got := ops(res); !reflect.DeepEqual(got, want) || !res.Applied {
		t.Fatalf("first run ops = %v applied=%v, want %v", got, res.Applied, want)
	}
	if n := f.count(t, `SELECT count(*) FROM sage.policy_sets
		WHERE name IN ('prod-strict', 'stage') AND managed_by = 'config'`); n != 2 {
		t.Fatalf("config-owned sets = %d, want 2", n)
	}
	if got, want := f.ceiling(t, "prod-strict"), f.ceiling(t, "default")/2; got != want {
		t.Fatalf("prod-strict ceiling = %v, want the file's %v", got, want)
	}
	if n := f.count(t, `SELECT count(*) FROM sage.guard_principals
		WHERE name = 'ci-bot' AND managed_by = 'config' AND env_ceiling = 'dev'
		  AND created_by LIKE 'config:principals/ci-bot.yaml@%'`); n != 1 {
		t.Fatalf("config principal rows = %d, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM sage.mcp_tokens`); n != 0 {
		t.Fatalf("a file minted %d tokens", n)
	}
	if n := f.count(t, `SELECT count(*) FROM sage.user_database_grants
		WHERE source = 'config' AND database_name = 'orders'`); n != 2 {
		t.Fatalf("config grants = %d, want 2", n)
	}
	if n := f.count(t, `SELECT count(*) FROM sage.auth_audit
		WHERE event = 'config_directory_reconciled' AND detail->>'hash' = $1`,
		res.Hash); n != 1 {
		t.Fatalf("reconcile audit entries with the hash = %d, want 1", n)
	}
	before := f.rowCounts(t)
	again := f.apply(t, root)
	if len(again.Changes) != 0 || again.Applied || again.Hash != res.Hash {
		t.Fatalf("second run = %+v, want a no-op with the same hash", again)
	}
	if after := f.rowCounts(t); !reflect.DeepEqual(before, after) {
		t.Fatalf("second run wrote rows: %v -> %v", before, after)
	}
}

func TestReconcile_InvalidInputWritesNothing(t *testing.T) {
	f := newFixture(t)
	store := agentguard.NewStore(f.pool)
	if _, err := store.Create(context.Background(), agentguard.CreateRequest{
		Name: "api-bot", Profile: "readonly-analyst", CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, file, body, text string
		line                   int
	}{
		{"unknown database", "grants/g.yaml",
			grantFile([3]string{"alice@example.com", "ledger", "viewer"}), "ledger", 4},
		{"unknown user", "grants/g.yaml",
			grantFile([3]string{"zed@example.com", "orders", "viewer"}), "zed@", 4},
		{"API principal without adopt", "principals/p.yaml",
			principalFile("api-bot", "readonly-analyst", "dev"), "adopt: true", 3},
		{"API set without adopt", "policy-sets/s.yaml",
			setFile("prod", 10, "prod", nil), "adopt: true", 3},
		{"sponsor who is not an operator", "principals/p.yaml",
			principalFile("new-bot", "readonly-analyst", "dev",
				"sponsor: alice@example.com\n"), "sponsor", 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := f.happyFiles()
			files[tc.file] = tc.body
			before := f.rowCounts(t)
			_, err := f.reconcile(t, writeTree(t, files), ModeApply)
			wantProblem(t, err, tc.file, tc.line, tc.text)
			if after := f.rowCounts(t); !reflect.DeepEqual(before, after) {
				t.Fatalf("an invalid directory wrote rows: %v -> %v", before, after)
			}
		})
	}
}

// CHECK-02: an API-owned principal and grant are never changed or retired.
func TestReconcile_EmptyDirectoryRetiresOnlyConfigRows(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	apiBot, err := agentguard.NewStore(f.pool).Create(ctx, agentguard.CreateRequest{
		Name: "api-bot", Profile: "readonly-analyst", CreatedBy: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.NewDatabaseGrantStore(f.pool).SetGrant(ctx, auth.GrantChange{
		ActorID: f.users["carol@example.com"], UserID: f.users["bob@example.com"],
		Database: "billing", Role: "viewer"}); err != nil {
		t.Fatal(err)
	}
	untouched := f.apiRows(t)
	// prod-strict without its narrower document: retiring a set whose
	// databases would fall back to a wider document is held (see
	// TestReconcile_RenameAndHeldRetire), and this test is about retiring.
	files := f.happyFiles()
	files["policy-sets/prod-strict.yaml"] = setFile("prod-strict", 50, "prod", nil)
	f.apply(t, writeTree(t, files))
	res := f.apply(t, t.TempDir())
	want := map[string]int{"policy_set:retire": 2, "principal:retire": 1, "grant:retire": 2}
	if got := ops(res); !reflect.DeepEqual(got, want) {
		t.Fatalf("empty directory ops = %v, want %v", got, want)
	}
	if n := f.count(t, `SELECT count(*) FROM sage.policy_sets
		WHERE managed_by = 'config'`); n != 0 {
		t.Fatalf("config sets left = %d", n)
	}
	if n := f.count(t, `SELECT count(*) FROM sage.guard_principals
		WHERE name = 'ci-bot' AND status = 'retired'`); n != 1 {
		t.Fatal("the config principal was not retired")
	}
	if n := f.count(t, `SELECT count(*) FROM sage.user_database_grants
		WHERE source = 'config'`); n != 0 {
		t.Fatalf("config grants left = %d", n)
	}
	if got := f.apiRows(t); got != untouched {
		t.Fatalf("API rows changed:\n%s\n%s", untouched, got)
	}
	p, err := agentguard.NewStore(f.pool).Get(ctx, apiBot.ID)
	if err != nil || p.Status != agentguard.StatusActive {
		t.Fatalf("API principal = %+v, %v", p, err)
	}
}

// CHECK-03: dry_run (and a follower) writes zero rows.
func TestReconcile_DryRunAndFollowerWriteZeroRows(t *testing.T) {
	f := newFixture(t)
	dir, err := Load(writeTree(t, f.happyFiles()))
	if err != nil {
		t.Fatal(err)
	}
	before := f.rowCounts(t)
	res, err := Reconcile(context.Background(), f.pool, dir,
		Options{Mode: ModeDryRun, Databases: monitored, Leader: true})
	if err != nil || res.Applied || len(res.Changes) != 6 || res.Mode != ModeDryRun {
		t.Fatalf("dry run = %+v, %v; want 6 changes, nothing applied", res, err)
	}
	follower, err := Reconcile(context.Background(), f.pool, dir,
		Options{Mode: ModeApply, Databases: monitored, Leader: false})
	if err != nil || follower.Applied || !follower.Follower || len(follower.Changes) != 6 {
		t.Fatalf("follower = %+v, %v; want the diff, read only", follower, err)
	}
	if after := f.rowCounts(t); !reflect.DeepEqual(before, after) {
		t.Fatalf("dry run or follower wrote rows: %v -> %v", before, after)
	}
	if _, err := Reconcile(context.Background(), f.pool, dir,
		Options{Mode: "yolo", Databases: monitored, Leader: true}); err == nil {
		t.Fatal("an unknown mode ran")
	}
	ctx := context.Background()
	if _, err := Reconcile(ctx, nil, dir, Options{Mode: ModeApply}); err == nil {
		t.Fatal("a reconcile without a control database ran")
	}
	if _, err := Reconcile(ctx, f.pool, nil, Options{Mode: ModeApply}); err == nil {
		t.Fatal("a reconcile without a directory ran")
	}
}

// CHECK-04: a widening from a file waits for a person; a narrowing applies.
func TestReconcile_PrincipalWideningWaitsForAPerson(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	files := f.happyFiles()
	f.apply(t, writeTree(t, files))
	files["principals/ci-bot.yaml"] = principalFile("ci-bot", "readonly-analyst", "prod")
	root := writeTree(t, files)
	res := f.apply(t, root)
	if got := ops(res); got["principal:propose"] != 1 || len(got) != 1 {
		t.Fatalf("raising the ceiling = %v, want one proposal", got)
	}
	ceiling := func() string {
		var env string
		if err := f.pool.QueryRow(ctx, `SELECT env_ceiling FROM sage.guard_principals
			WHERE name = 'ci-bot'`).Scan(&env); err != nil {
			t.Fatal(err)
		}
		return env
	}
	if ceiling() != "dev" {
		t.Fatal("a widening took effect from a file alone")
	}
	if n := f.count(t, `SELECT count(*) FROM sage.guard_principal_changes
		WHERE status = 'pending' AND requested_by LIKE 'config:principals/ci-bot.yaml@%'
		  AND requested_by_user = 0`); n != 1 {
		t.Fatalf("pending config changes = %d, want 1", n)
	}
	again := f.apply(t, root)
	if got := ops(again); got["principal:waiting"] != 1 || again.Applied {
		t.Fatalf("second run = %v applied=%v, want waiting and no write", got, again.Applied)
	}
	if n := f.count(t, `SELECT count(*) FROM sage.guard_principal_changes`); n != 1 {
		t.Fatalf("change rows = %d after a second run, want 1", n)
	}
	confirmByPerson(t, f)
	if ceiling() != "prod" {
		t.Fatal("the person's confirm did not apply the widening")
	}
	files["principals/ci-bot.yaml"] = principalFile("ci-bot", "readonly-analyst", "dev")
	lower := f.apply(t, writeTree(t, files))
	if got := ops(lower); got["principal:update"] != 1 || ceiling() != "dev" {
		t.Fatalf("lowering = %v, ceiling %s; want an update applied at once", got, ceiling())
	}
}

func confirmByPerson(t *testing.T, f *fixture) {
	t.Helper()
	ctx := context.Background()
	var id string
	var change int64
	if err := f.pool.QueryRow(ctx, `SELECT principal_id, id FROM sage.guard_principal_changes
		WHERE status = 'pending'`).Scan(&id, &change); err != nil {
		t.Fatal(err)
	}
	prod := agentguard.EnvProd
	carol := f.users["carol@example.com"]
	if _, err := agentguard.NewStore(f.pool).ConfirmChange(ctx, agentguard.ChangeConfirm{
		PrincipalID: id, ChangeID: change, Patch: agentguard.Patch{EnvCeiling: &prod},
		Actor: "carol@example.com", Audit: agentguard.Audit{ActorUserID: carol}}); err != nil {
		t.Fatalf("a person's confirm: %v", err)
	}
}

func TestReconcile_DocumentWideningIsOnlyProposed(t *testing.T) {
	f := newFixture(t)
	files := f.happyFiles()
	f.apply(t, writeTree(t, files))
	narrow := f.ceiling(t, "prod-strict")
	files["policy-sets/prod-strict.yaml"] = setFile("prod-strict", 50, "prod", f.wide)
	root := writeTree(t, files)
	res := f.apply(t, root)
	if got := ops(res); got["policy_set:propose"] != 1 || len(got) != 1 {
		t.Fatalf("a wider document = %v, want one proposal", got)
	}
	if f.ceiling(t, "prod-strict") != narrow {
		t.Fatal("a wider document took effect from a file alone")
	}
	again := f.apply(t, root)
	if got := ops(again); got["policy_set:waiting"] != 1 || again.Applied {
		t.Fatalf("second run = %v, want waiting", got)
	}
	var proposal int64
	if err := f.pool.QueryRow(context.Background(), `SELECT id FROM sage.policy
		WHERE set_name = 'prod-strict' AND status = 'proposed'`).Scan(&proposal); err != nil {
		t.Fatalf("one pending proposal: %v", err)
	}
	var version int64
	if err := f.pool.QueryRow(context.Background(), `SELECT version FROM sage.policy
		WHERE set_name = 'prod-strict' AND status = 'active'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.NewStore(f.pool).Ratify(context.Background(), policy.RatifyRequest{
		ProposalID: proposal, ExpectedVersion: version, Actor: "carol@example.com",
		ApproverUserID: f.users["carol@example.com"]}); err != nil {
		t.Fatalf("a person's ratify: %v", err)
	}
	if f.ceiling(t, "prod-strict") != narrow*4 {
		t.Fatal("the ratified document is not in force")
	}
	if got := f.apply(t, root); len(got.Changes) != 0 {
		t.Fatalf("after the ratify the file matches; changes = %v", got.Changes)
	}
}

// Boundary: a rename is a retire plus a create; retiring a set whose
// databases would fall back to a wider document is held.
func TestReconcile_RenameAndHeldRetire(t *testing.T) {
	f := newFixture(t)
	files := f.happyFiles()
	f.apply(t, writeTree(t, files))
	delete(files, "policy-sets/prod-strict.yaml")
	files["policy-sets/prod-tight.yaml"] = setFile("prod-tight", 50, "prod", f.narrow)
	res := f.apply(t, writeTree(t, files))
	got := ops(res)
	if got["policy_set:retire"] != 1 || got["policy_set:create"] != 1 ||
		got["policy_set:ratify"] != 1 {
		t.Fatalf("rename ops = %v, want retire + create + ratify", got)
	}
	if n := f.count(t, `SELECT count(*) FROM sage.policy_sets WHERE name = 'prod-strict'`); n != 0 {
		t.Fatal("the old name is still a set")
	}
	delete(files, "policy-sets/prod-tight.yaml")
	held := f.apply(t, writeTree(t, files))
	if g := ops(held); g["policy_set:hold"] != 1 || held.Applied {
		t.Fatalf("retiring the narrow set = %v, want held", g)
	}
	if !strings.Contains(held.Changes[0].Detail, "orders") {
		t.Fatalf("hold detail %q does not name the database", held.Changes[0].Detail)
	}
	if n := f.count(t, `SELECT count(*) FROM sage.policy_sets WHERE name = 'prod-tight'`); n != 1 {
		t.Fatal("a widening retire took effect")
	}
}

func TestReconcile_DatabaseErrorRollsBackWhole(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `
CREATE FUNCTION sage.test_refuse() RETURNS trigger LANGUAGE plpgsql AS
$$ BEGIN RAISE EXCEPTION 'grant writes are broken'; END $$;
CREATE TRIGGER test_refuse BEFORE INSERT ON sage.user_database_grants
FOR EACH ROW EXECUTE FUNCTION sage.test_refuse()`); err != nil {
		t.Fatal(err)
	}
	before := f.rowCounts(t)
	_, err := f.reconcile(t, writeTree(t, f.happyFiles()), ModeApply)
	if err == nil || !strings.Contains(err.Error(), "grant writes are broken") ||
		!strings.Contains(err.Error(), "grants/orders.yaml") {
		t.Fatalf("error = %v, want the database's error with the file", err)
	}
	var invalid *InvalidError
	if errors.As(err, &invalid) {
		t.Fatal("a database failure reported as invalid input")
	}
	if after := f.rowCounts(t); !reflect.DeepEqual(before, after) {
		t.Fatalf("a failed reconcile left rows: %v -> %v", before, after)
	}
}

// Two replicas reconciling at once: the sets lock serializes them and
// the second sees the first's commit, so each row is written once.
func TestReconcile_ConcurrentReplicasWriteOnce(t *testing.T) {
	f := newFixture(t)
	dir, err := Load(writeTree(t, f.happyFiles()))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]Result, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = Reconcile(context.Background(), f.pool, dir,
				Options{Mode: ModeApply, Databases: monitored, Leader: true})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("replica %d: %v", i, err)
		}
	}
	if results[0].Applied == results[1].Applied {
		t.Fatalf("applied = %v %v, want exactly one writer", results[0].Applied,
			results[1].Applied)
	}
	if n := f.count(t, `SELECT count(*) FROM sage.auth_audit
		WHERE event = 'config_directory_reconciled'`); n != 1 {
		t.Fatalf("reconcile audit entries = %d, want 1", n)
	}
	if n := f.count(t, `SELECT count(*) FROM sage.guard_principals`); n != 1 {
		t.Fatalf("principals = %d, want 1", n)
	}
}

// State transitions: dry_run shows, apply writes, back to dry_run stops
// writes; adopting an API row needs adopt: true.
func TestReconcile_ModeTransitionsAndAdopt(t *testing.T) {
	f := newFixture(t)
	files := f.happyFiles()
	root := writeTree(t, files)
	if res, err := f.reconcile(t, root, ModeDryRun); err != nil || len(res.Changes) != 6 {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	f.apply(t, root)
	files["grants/orders.yaml"] = grantFile(
		[3]string{"alice@example.com", "orders", "operator"},
		[3]string{"bob@example.com", "orders", "operator"})
	root = writeTree(t, files)
	before := f.rowCounts(t)
	res, err := f.reconcile(t, root, ModeDryRun)
	if err != nil || ops(res)["grant:update"] != 1 || res.Applied {
		t.Fatalf("dry run after apply = %+v, %v", res, err)
	}
	if after := f.rowCounts(t); !reflect.DeepEqual(before, after) {
		t.Fatal("dry run after apply wrote rows")
	}
	if _, err := agentguard.NewStore(f.pool).Create(context.Background(),
		agentguard.CreateRequest{Name: "legacy-bot", Profile: "readonly-analyst",
			CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	files["principals/legacy.yaml"] = principalFile("legacy-bot", "readonly-analyst",
		"dev", "adopt: true\n")
	adopted := f.apply(t, writeTree(t, files))
	if got := ops(adopted); got["principal:adopt"] != 1 || got["grant:update"] != 1 {
		t.Fatalf("adopt run = %v", got)
	}
	if n := f.count(t, `SELECT count(*) FROM sage.guard_principals
		WHERE name = 'legacy-bot' AND managed_by = 'config'`); n != 1 {
		t.Fatal("adopt: true did not move the principal to config ownership")
	}
}
