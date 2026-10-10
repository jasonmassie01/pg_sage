package envbind

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
)

// G1-04: an agent's claimed environment is ignored and an unverified
// binding is prod. G1-04b: a clone or standby of prod registered as dev
// without a receipt, or a re-pointed DSN, is evaluated as prod and raises
// a critical finding.

func TestDatabaseCarriesNoClaimedEnvironment(t *testing.T) {
	// The environment is derived from the stored label and the live
	// identity only: nothing an agent sends can name it.
	var fields []string
	typ := reflect.TypeOf(Database{})
	for i := range typ.NumField() {
		fields = append(fields, typ.Field(i).Name)
	}
	if !slices.Equal(fields, []string{"ID", "Name", "Pool", "ProviderRef"}) {
		t.Fatalf("Database fields %v: an environment input would let a claim through", fields)
	}
}

func TestEnvironmentOf_DefaultProd(t *testing.T) {
	control, ctx := controlPool(t)
	app := extraPool(t, ctx, "envdefault")
	db := Database{ID: newID(t, control, app), Name: "app", Pool: app}
	got, err := NewBinder(control, nil).EnvironmentOf(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	ev := got.Evidence
	if got.Env != EnvProd || ev.Label != EnvProd || ev.Verified ||
		!slices.Equal(ev.Reasons, []string{ReasonDefaultProd}) {
		t.Fatalf("unlabelled database: %+v", got)
	}
	if ev.Live.SystemIdentifier == "" || ev.Live.DBOID == 0 ||
		!strings.HasSuffix(ev.Live.Target, "/"+app.Config().ConnConfig.Database) ||
		ev.Strength != StrengthCluster || ev.DatabaseID != db.ID {
		t.Fatalf("live identity evidence: %+v", ev)
	}
}

func TestEnvironmentOf_NoControlOrUnbound(t *testing.T) {
	control, ctx := controlPool(t)
	got, err := NewBinder(nil, nil).EnvironmentOf(ctx, Database{ID: "x", Pool: control})
	if err != nil || got.Env != EnvProd ||
		!slices.Equal(got.Evidence.Reasons, []string{ReasonNoControl}) {
		t.Fatalf("no control database: %+v %v", got, err)
	}
	got, err = NewBinder(control, nil).EnvironmentOf(ctx, Database{Name: "a", Pool: control})
	if err != nil || got.Env != EnvProd ||
		!slices.Equal(got.Evidence.Reasons, []string{ReasonUnbound}) {
		t.Fatalf("no database_id: %+v %v", got, err)
	}
}

func TestEnvironmentOf_UnreachableIsProdWithError(t *testing.T) {
	control, ctx := controlPool(t)
	dead, err := pgxpool.New(ctx,
		"postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer dead.Close()
	id := newID(t, control)
	got, err := NewBinder(control, nil).EnvironmentOf(ctx, Database{ID: id, Pool: dead})
	if err == nil || got.Env != EnvProd ||
		!slices.Contains(got.Evidence.Reasons, ReasonIdentityUnreadable) {
		t.Fatalf("unreachable database: %+v %v", got, err)
	}
}

// labelDev widens a database to dev with two admins.
func labelDev(t *testing.T, ctx context.Context, b *Binder, db Database, env Env) {
	t.Helper()
	first, err := b.SetLabel(ctx, db, env, "alice@example.com")
	if err != nil || first.Applied || !first.Pending {
		t.Fatalf("first admin: %+v %v", first, err)
	}
	second, err := b.SetLabel(ctx, db, env, "bob@example.com")
	if err != nil || !second.Applied || second.Record.Label != env {
		t.Fatalf("second admin: %+v %v", second, err)
	}
}

func TestSetLabel_WideningNeedsTwoAdmins(t *testing.T) {
	control, ctx := controlPool(t)
	app := extraPool(t, ctx, "envwiden")
	db := Database{ID: newID(t, control, app), Name: "app", Pool: app}
	b := NewBinder(control, nil)
	again, err := b.SetLabel(ctx, db, EnvDev, "alice@example.com")
	if err != nil || !again.Pending {
		t.Fatalf("first request: %+v %v", again, err)
	}
	again, err = b.SetLabel(ctx, db, EnvDev, "alice@example.com")
	if err != nil || again.Applied || !again.Pending {
		t.Fatalf("the same admin twice must stay pending: %+v %v", again, err)
	}
	if got, _ := b.EnvironmentOf(ctx, db); got.Env != EnvProd {
		t.Fatalf("pending widening already effective: %+v", got)
	}
	other, err := b.SetLabel(ctx, db, EnvStage, "bob@example.com")
	if err != nil || other.Applied || !other.Pending || other.Record.PendingLabel != EnvStage {
		t.Fatalf("a different label restarts the request: %+v %v", other, err)
	}
	done, err := b.SetLabel(ctx, db, EnvStage, "alice@example.com")
	if err != nil || !done.Applied {
		t.Fatalf("second admin for stage: %+v %v", done, err)
	}
	got, err := b.EnvironmentOf(ctx, db)
	if err != nil || got.Env != EnvStage || !got.Evidence.Verified ||
		got.Evidence.SetBy != "alice@example.com" {
		t.Fatalf("after two admins: %+v %v", got, err)
	}
	narrow, err := b.SetLabel(ctx, db, EnvProd, "carol@example.com")
	if err != nil || !narrow.Applied || narrow.Pending {
		t.Fatalf("narrowing is immediate: %+v %v", narrow, err)
	}
	if got, _ := b.EnvironmentOf(ctx, db); got.Env != EnvProd {
		t.Fatalf("after narrowing: %+v", got)
	}
}

func TestSetLabel_PendingExpires(t *testing.T) {
	control, ctx := controlPool(t)
	app := extraPool(t, ctx, "envpending")
	db := Database{ID: newID(t, control, app), Name: "app", Pool: app}
	b := NewBinder(control, nil, WithPendingTTL(time.Second))
	if _, err := b.SetLabel(ctx, db, EnvDev, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := control.Exec(ctx, `UPDATE sage.guard_environment_labels
		SET pending_at = now() - interval '1 hour' WHERE database_id = $1`, db.ID); err != nil {
		t.Fatal(err)
	}
	res, err := b.SetLabel(ctx, db, EnvDev, "bob@example.com")
	if err != nil || res.Applied || !res.Pending || res.Record.PendingBy != "bob@example.com" {
		t.Fatalf("an expired request does not count as the first approval: %+v %v", res, err)
	}
}

func TestSetLabel_Invalid(t *testing.T) {
	control, ctx := controlPool(t)
	db := Database{ID: newID(t, control), Name: "app", Pool: control}
	b := NewBinder(control, nil)
	if _, err := b.SetLabel(ctx, db, Env("qa"), "a@example.com"); !errors.Is(err, ErrInvalidEnv) {
		t.Errorf("label qa: %v", err)
	}
	if _, err := b.SetLabel(ctx, db, EnvDev, " "); !errors.Is(err, ErrInvalidActor) {
		t.Errorf("blank actor: %v", err)
	}
	if _, err := NewBinder(nil, nil).SetLabel(ctx, db, EnvDev, "a@example.com"); !errors.Is(
		err, ErrNoControl) {
		t.Errorf("no control: %v", err)
	}
	if _, err := b.SetLabel(ctx, Database{Pool: control}, EnvDev, "a@example.com"); !errors.Is(
		err, ErrUnbound) {
		t.Errorf("no database_id: %v", err)
	}
}

func findingCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.findings
		WHERE category = $1 AND object_identifier = $2 AND status = 'open'
		  AND severity = 'critical'`, FindingCategory, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRepointedDSNIsProdWithCriticalFinding(t *testing.T) {
	control, ctx := controlPool(t)
	dev := extraPool(t, ctx, "envdev")
	prod := extraPool(t, ctx, "envprod")
	id := newID(t, control, dev, prod)
	b := NewBinder(control, nil)
	labelDev(t, ctx, b, Database{ID: id, Name: "dev", Pool: dev}, EnvDev)
	// The same configuration entry (same database_id), DSN now at another
	// database.
	moved := Database{ID: id, Name: "dev", Pool: prod}
	got, err := b.EnvironmentOf(ctx, moved)
	if err != nil || got.Env != EnvProd || !got.Evidence.Critical ||
		!slices.Contains(got.Evidence.Reasons, ReasonBindingChanged) ||
		!slices.Contains(got.Evidence.Changed, "db_oid") {
		t.Fatalf("re-pointed DSN: %+v %v", got, err)
	}
	rep, err := b.Reconcile(ctx, []Database{moved}, Fence{})
	if err != nil || !slices.Contains(rep.Critical, "dev") {
		t.Fatalf("reconcile: %+v %v", rep, err)
	}
	if n := findingCount(t, ctx, prod, id); n != 1 {
		t.Fatalf("critical findings %d, want 1", n)
	}
	if _, err := b.Reconcile(ctx, []Database{moved}, Fence{}); err != nil {
		t.Fatal(err)
	}
	if n := findingCount(t, ctx, prod, id); n != 1 {
		t.Fatalf("a second pass must not duplicate the finding: %d", n)
	}
	// Re-labelling (narrowing to prod) resolves it.
	if _, err := b.SetLabel(ctx, moved, EnvProd, "carol@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reconcile(ctx, []Database{moved}, Fence{}); err != nil {
		t.Fatal(err)
	}
	if n := findingCount(t, ctx, prod, id); n != 0 {
		t.Fatalf("finding still open after the operator re-labelled: %d", n)
	}
}

func TestCloneOfProdRegisteredAsDev(t *testing.T) {
	// Two configuration entries reaching one physical database (a
	// pg_basebackup copy or standby looks exactly like this: the same
	// system identifier and database OID).
	control, ctx := controlPool(t)
	phys := extraPool(t, ctx, "envclone")
	devDB := Database{ID: newID(t, control, phys), Name: "copy", Pool: phys}
	prodDB := Database{ID: newID(t, control, phys), Name: "primary", Pool: phys}
	b := NewBinder(control, nil)
	labelDev(t, ctx, b, devDB, EnvDev) // before the prod entry was observed
	rep, err := b.Reconcile(ctx, []Database{devDB, prodDB}, Fence{})
	if err != nil || rep.Observed != 2 {
		t.Fatalf("reconcile: %+v %v", rep, err)
	}
	for _, db := range []Database{devDB, prodDB} {
		got, err := b.EnvironmentOf(ctx, db)
		if err != nil || got.Env != EnvProd || !got.Evidence.Critical ||
			!slices.Contains(got.Evidence.Reasons, ReasonTwoLabels) {
			t.Fatalf("%s: %+v %v", db.Name, got, err)
		}
		if n := findingCount(t, ctx, phys, db.ID); n != 1 {
			t.Errorf("%s: critical findings %d", db.Name, n)
		}
	}
	_, err = b.SetLabel(ctx, devDB, EnvDev, "bob@example.com")
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Reason != ReasonTwoLabels {
		t.Fatalf("labelling the copy dev again: %v", err)
	}
}

func TestBranchLabelNeedsActiveReceipt(t *testing.T) {
	control, ctx := controlPool(t)
	sbx := extraPool(t, ctx, "envbranch")
	db := Database{ID: newID(t, control, sbx), Name: "sbx-" + newID(t, control)[:8], Pool: sbx}
	receipts := clone.NewReceiptStore(control)
	b := NewBinder(control, receipts)
	_, err := b.SetLabel(ctx, db, EnvBranch, "alice@example.com")
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Reason != ReasonReceiptMissing {
		t.Fatalf("branch without receipt: %v", err)
	}
	r, err := receipts.Record(ctx, clone.Receipt{DeploymentID: "00000000-0000-4000-8000-00000000d001",
		Adapter: "dle", Scope: "test", Name: db.Name, Purpose: clone.PurposeSandbox,
		ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = control.Exec(context.Background(),
			`DELETE FROM sage.clone_instances WHERE id = $1`, r.ID)
	})
	if _, err := b.SetLabel(ctx, db, EnvBranch, "alice@example.com"); !errors.As(err,
		&refused) {
		t.Fatalf("a receipt still creating is not active: %v", err)
	}
	if _, err := receipts.MarkReady(ctx, r.ID, "clone-42"); err != nil {
		t.Fatal(err)
	}
	labelDev(t, ctx, b, db, EnvBranch)
	got, err := b.EnvironmentOf(ctx, db)
	if err != nil || got.Env != EnvBranch || got.Evidence.Live.ProviderRef != "dle:clone-42" ||
		got.Evidence.Receipt == nil || got.Evidence.Receipt.ID != r.ID {
		t.Fatalf("receipt-backed branch: %+v %v", got, err)
	}
	if _, err := control.Exec(ctx, `UPDATE sage.clone_instances
		SET expires_at = now() - interval '1 second' WHERE id = $1`, r.ID); err != nil {
		t.Fatal(err)
	}
	got, err = b.EnvironmentOf(ctx, db)
	if err != nil || got.Env != EnvProd ||
		!slices.Contains(got.Evidence.Reasons, ReasonReceiptMissing) {
		t.Fatalf("expired receipt: %+v %v", got, err)
	}
}

func TestUnreadableSystemIdentifierIsUnverifiable(t *testing.T) {
	control, ctx := controlPool(t)
	locked := extraPool(t, ctx, "envlocked")
	if _, err := locked.Exec(ctx,
		`REVOKE EXECUTE ON FUNCTION pg_catalog.pg_control_system() FROM PUBLIC`); err != nil {
		t.Fatal(err)
	}
	role := "envbind_ro_" + strings.ReplaceAll(newID(t, control)[:8], "-", "")
	if _, err := control.Exec(ctx, `CREATE ROLE `+role+` LOGIN PASSWORD 'pw'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = control.Exec(context.Background(), `DROP ROLE `+role) })
	cfg := locked.Config().Copy()
	cfg.ConnConfig.User, cfg.ConnConfig.Password = role, "pw"
	ro, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	db := Database{ID: newID(t, control), Name: "locked", Pool: ro}
	b := NewBinder(control, nil)
	got, err := b.EnvironmentOf(ctx, db)
	if err != nil || got.Env != EnvProd || got.Evidence.Live.SystemIdentifier != "" ||
		got.Evidence.Live.DBOID == 0 || got.Evidence.Strength != StrengthConfigured {
		t.Fatalf("unreadable system identifier: %+v %v", got, err)
	}
	_, err = b.SetLabel(ctx, db, EnvDev, "alice@example.com")
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Reason != ReasonUnverifiable {
		t.Fatalf("dev label without a physical identity: %v", err)
	}
	db.ProviderRef = "rds:dev-instance"
	if _, err := b.SetLabel(ctx, db, EnvDev, "alice@example.com"); err != nil {
		t.Fatalf("a provider resource id verifies the binding: %v", err)
	}
}

func TestReconcile_FencedAndFillsSREBinding(t *testing.T) {
	control, ctx := controlPool(t)
	app := extraPool(t, ctx, "envfence")
	id := newID(t, control, app)
	db := Database{ID: id, Name: "app", Pool: app}
	if _, err := control.Exec(ctx, `INSERT INTO sage.sre_deployments (deployment_id)
		VALUES (gen_random_uuid()) ON CONFLICT (singleton) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := control.Exec(ctx, `INSERT INTO sage.sre_database_bindings
		(deployment_id, database_id, runtime_key, identity_strength, cluster_epoch)
		SELECT deployment_id, $1, $2, 'configured', 'unknown'
		FROM sage.sre_deployments`, id, "envtest:"+id); err != nil {
		t.Fatal(err)
	}
	scope := "envtest:" + id
	t.Cleanup(func() {
		c := context.Background()
		_, _ = control.Exec(c, `DELETE FROM sage.sre_database_bindings WHERE database_id = $1`, id)
		_, _ = control.Exec(c, `DELETE FROM sage.fleet_leader_lease WHERE scope = $1`, scope)
	})
	if _, err := control.Exec(ctx, `INSERT INTO sage.fleet_leader_lease
		(scope, holder, epoch, acquired_at, renewed_at, expires_at)
		VALUES ($1, 'h1', 2, now(), now(), now() + interval '1 minute')`, scope); err != nil {
		t.Fatal(err)
	}
	b := NewBinder(control, nil)
	_, err := b.Reconcile(ctx, []Database{db}, Fence{Scope: scope, Holder: "h1", Epoch: 1})
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("stale epoch: %v", err)
	}
	if _, ok, _ := b.Record(ctx, id); ok {
		t.Fatal("a fenced-out reconcile wrote a label row")
	}
	rep, err := b.Reconcile(ctx, []Database{db}, Fence{Scope: scope, Holder: "h1", Epoch: 2})
	if err != nil || rep.Observed != 1 {
		t.Fatalf("current epoch: %+v %v", rep, err)
	}
	rec, ok, err := b.Record(ctx, id)
	if err != nil || !ok || rec.Label != EnvProd || rec.Observed == nil ||
		rec.Observed.SystemIdentifier == "" || rec.SetBy != DefaultSetBy {
		t.Fatalf("observed row: %+v %v %v", rec, ok, err)
	}
	var strength, epoch string
	if err := control.QueryRow(ctx, `SELECT identity_strength, cluster_epoch
		FROM sage.sre_database_bindings WHERE database_id = $1`, id).Scan(&strength,
		&epoch); err != nil {
		t.Fatal(err)
	}
	if strength != "cluster" || epoch != rec.Observed.PhysicalKey() {
		t.Fatalf("sre binding strength %q epoch %q, want cluster %q", strength, epoch,
			rec.Observed.PhysicalKey())
	}
}

func TestConcurrentLabelAndEvaluate(t *testing.T) {
	control, ctx := controlPool(t)
	app := extraPool(t, ctx, "envconc")
	db := Database{ID: newID(t, control, app), Name: "app", Pool: app}
	b := NewBinder(control, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	actors := []string{"a@example.com", "b@example.com", "c@example.com", "d@example.com"}
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				if _, err := b.SetLabel(ctx, db, EnvDev, actors[i%4]); err != nil {
					errs <- err
				}
				return
			}
			got, err := b.EnvironmentOf(ctx, db)
			if err != nil {
				errs <- err
			} else if got.Env != EnvProd && got.Env != EnvDev {
				errs <- errors.New("impossible environment " + string(got.Env))
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	got, err := b.EnvironmentOf(ctx, db)
	if err != nil || got.Env != EnvDev {
		t.Fatalf("after concurrent approvals by distinct admins: %+v %v", got, err)
	}
}
