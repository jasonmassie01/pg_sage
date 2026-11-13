package plan

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// pkFacts is public.orders.id integer, the primary key, filled from its
// own sequence and referenced by one foreign key: the int -> bigint case.
func pkFacts() SwapFacts {
	return SwapFacts{
		Schema: "public", Table: "orders", RelKind: "r", EstimatedRows: 1_000_000,
		ServerVersion: 170000,
		Column: SwapColumn{Name: "id", Type: "integer", NotNull: true,
			Default:      `nextval('orders_id_seq'::regclass)`,
			DefaultIsSeq: true, SequenceSchema: "public", SequenceName: "orders_id_seq",
			SequenceType: "integer"},
		PrimaryKey: &SwapKey{Constraint: "orders_pkey", Columns: []string{"id"},
			ColumnType: "integer"},
		Referencing: []SwapForeignKey{{Constraint: "lines_order_fk", Schema: "public",
			Table: "lines", Column: "order_id", ColumnType: "integer", Columns: 1,
			OnUpdate: "a", OnDelete: "c", Match: "s"}},
	}
}

// textFacts is public.events.ref text, nullable, keyed by id bigint: the
// type tightening (text -> bigint) the schema guard recommends.
func textFacts() SwapFacts {
	return SwapFacts{
		Schema: "public", Table: "events", RelKind: "r", ServerVersion: 140000,
		Column: SwapColumn{Name: "ref", Type: "text"},
		PrimaryKey: &SwapKey{Constraint: "events_pkey", Columns: []string{"id"},
			ColumnType: "bigint"},
	}
}

func mustSwap(t *testing.T, facts SwapFacts, req SwapRequest) SwapPlan {
	t.Helper()
	plan, err := PlanColumnSwap(context.Background(), facts, req)
	if err != nil {
		t.Fatalf("PlanColumnSwap: %v", err)
	}
	return plan
}

func stepNames(p SwapPlan) []string {
	names := make([]string, len(p.Steps))
	for i, s := range p.Steps {
		names[i] = s.Name
	}
	return names
}

func stepByName(t *testing.T, p SwapPlan, name string) SwapStep {
	t.Helper()
	for _, s := range p.Steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("plan has no step %q: %v", name, stepNames(p))
	return SwapStep{}
}

func intReq() SwapRequest {
	return SwapRequest{Schema: "public", Table: "orders", Column: "id", TargetType: "bigint"}
}

func TestColumnSwapPrimaryKeyStepOrder(t *testing.T) {
	p := mustSwap(t, pkFacts(), intReq())
	want := []string{
		"set_lock_timeout", "set_plan_cache_mode", "add_column", "create_sync_function",
		"create_sync_trigger",
		"add_not_null_proof", "backfill", "validate_not_null_proof", "build_unique_index",
		"begin", "lock_tables", "drop_referencing_fk", "drop_primary_key",
		"drop_sync_trigger", "drop_old_default", "drop_old_not_null", "rename_old_column",
		"rename_new_column", "set_not_null", "drop_not_null_proof", "attach_primary_key",
		"set_default", "move_sequence", "add_referencing_fk", "commit",
		"validate_referencing_fk", "drop_sync_function", "drop_old_column",
	}
	if got := stepNames(p); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("steps\n got %v\nwant %v", got, want)
	}
	if !p.RecommendOnly || p.FromType != "integer" || p.ToType != "bigint" {
		t.Fatalf("plan header %+v", p)
	}
}

// Every step states a PostgreSQL lock mode or NONE, and the steps between
// BEGIN and COMMIT, only those, run in the swap transaction.
func TestColumnSwapLockLevelsAndTransaction(t *testing.T) {
	p := mustSwap(t, pkFacts(), intReq())
	want := map[string]string{
		"set_lock_timeout": LockNone, "add_column": "ACCESS EXCLUSIVE",
		"create_sync_function": LockNone, "create_sync_trigger": "SHARE ROW EXCLUSIVE",
		"add_not_null_proof": "ACCESS EXCLUSIVE", "backfill": "ROW EXCLUSIVE",
		"validate_not_null_proof": "SHARE UPDATE EXCLUSIVE",
		"build_unique_index":      "SHARE UPDATE EXCLUSIVE", "begin": LockNone,
		"lock_tables": "ACCESS EXCLUSIVE", "rename_old_column": "ACCESS EXCLUSIVE",
		"add_referencing_fk": "SHARE ROW EXCLUSIVE", "move_sequence": "ACCESS SHARE",
		"commit": LockNone, "validate_referencing_fk": "SHARE UPDATE EXCLUSIVE",
		"drop_sync_function": "ACCESS EXCLUSIVE", "drop_old_column": "ACCESS EXCLUSIVE",
		"drop_sync_trigger": "ACCESS EXCLUSIVE",
	}
	inTx := false
	for _, s := range p.Steps {
		if lock, ok := want[s.Name]; ok && s.LockLevel != lock {
			t.Errorf("%s lock %q, want %q", s.Name, s.LockLevel, lock)
		}
		if s.LockLevel == "" {
			t.Errorf("%s has no lock level", s.Name)
		}
		switch s.Name {
		case "begin":
			inTx = true
		case "commit":
			if !s.InTransaction {
				t.Errorf("commit is outside the transaction")
			}
			inTx = false
			continue
		}
		if s.InTransaction != inTx {
			t.Errorf("%s InTransaction %v, want %v", s.Name, s.InTransaction, inTx)
		}
	}
}

func TestColumnSwapPrimaryKeySQL(t *testing.T) {
	p := mustSwap(t, pkFacts(), intReq())
	checks := map[string]string{
		"set_lock_timeout":    `SET lock_timeout = '2000ms'`,
		"set_plan_cache_mode": `SET plan_cache_mode = force_custom_plan`,
		"add_column":          `ALTER TABLE "public"."orders" ADD COLUMN "id__swap_new" bigint`,
		"create_sync_trigger": `CREATE TRIGGER "orders_id_swap_sync" BEFORE INSERT OR UPDATE ` +
			`ON "public"."orders" FOR EACH ROW EXECUTE FUNCTION "public"."orders_id_swap_sync"()`,
		"add_not_null_proof": `ALTER TABLE "public"."orders" ADD CONSTRAINT "orders_id_swap_nn" ` +
			`CHECK ("id__swap_new" IS NOT NULL) NOT VALID`,
		"build_unique_index": `CREATE UNIQUE INDEX CONCURRENTLY "orders_id_swap_key" ` +
			`ON "public"."orders" ("id__swap_new")`,
		"lock_tables": `LOCK TABLE "public"."lines", "public"."orders" ` +
			`IN ACCESS EXCLUSIVE MODE`,
		"drop_referencing_fk": `ALTER TABLE "public"."lines" DROP CONSTRAINT "lines_order_fk"`,
		"drop_primary_key":    `ALTER TABLE "public"."orders" DROP CONSTRAINT "orders_pkey"`,
		"drop_sync_trigger":   `DROP TRIGGER "orders_id_swap_sync" ON "public"."orders"`,
		"rename_old_column": `ALTER TABLE "public"."orders" RENAME COLUMN "id" ` +
			`TO "id__swap_old"`,
		"attach_primary_key": `ALTER TABLE "public"."orders" ADD CONSTRAINT "orders_pkey" ` +
			`PRIMARY KEY USING INDEX "orders_id_swap_key"`,
		"set_default": `ALTER TABLE "public"."orders" ALTER COLUMN "id" SET DEFAULT ` +
			`nextval('"public"."orders_id_seq"'::regclass)`,
		"move_sequence": `ALTER SEQUENCE "public"."orders_id_seq" AS bigint ` +
			`OWNED BY "public"."orders"."id"`,
		"add_referencing_fk": `ALTER TABLE "public"."lines" ADD CONSTRAINT "lines_order_fk" ` +
			`FOREIGN KEY ("order_id") REFERENCES "public"."orders" ("id") ` +
			`ON DELETE CASCADE NOT VALID`,
		"validate_referencing_fk": `ALTER TABLE "public"."lines" VALIDATE CONSTRAINT ` +
			`"lines_order_fk"`,
		"drop_sync_function": `DROP FUNCTION "public"."orders_id_swap_sync"()`,
		"drop_old_column":    `ALTER TABLE "public"."orders" DROP COLUMN "id__swap_old"`,
	}
	for name, sql := range checks {
		if got := stepByName(t, p, name).SQL; got != sql {
			t.Errorf("%s\n got %s\nwant %s", name, got, sql)
		}
	}
	fn := stepByName(t, p, "create_sync_function").SQL
	for _, part := range []string{
		`CREATE FUNCTION "public"."orders_id_swap_sync"() RETURNS trigger LANGUAGE plpgsql`,
		`NEW."id__swap_new" := NEW."id"::bigint;`, `RETURN NEW;`,
	} {
		if !strings.Contains(fn, part) {
			t.Errorf("sync function lacks %q:\n%s", part, fn)
		}
	}
}

// The backfill walks the primary key in batches: $1 is the last key of
// the previous batch (NULL for the first), and each batch returns its keys.
func TestColumnSwapBackfillBatches(t *testing.T) {
	req := intReq()
	req.BatchSize = 250
	s := stepByName(t, mustSwap(t, pkFacts(), req), "backfill")
	want := `WITH batch AS (SELECT "id" FROM "public"."orders" ` +
		`WHERE $1::integer IS NULL OR "id" > $1::integer ORDER BY "id" LIMIT 250) ` +
		`UPDATE "public"."orders" AS t SET "id__swap_new" = t."id"::bigint ` +
		`FROM batch WHERE t."id" = batch."id" RETURNING t."id"`
	if s.SQL != want || !s.Repeat || !s.DML || s.InTransaction || s.Destructive {
		t.Fatalf("backfill step %+v\nwant SQL %s", s, want)
	}
}

// Text -> bigint on a nullable column with no key role: no key, default,
// sequence or NOT NULL steps, and the batches walk the table's own key.
func TestColumnSwapTextToBigint(t *testing.T) {
	p := mustSwap(t, textFacts(), SwapRequest{Schema: "public", Table: "events",
		Column: "ref", TargetType: "bigint"})
	want := []string{"set_lock_timeout", "set_plan_cache_mode", "add_column",
		"create_sync_function", "create_sync_trigger", "backfill", "begin", "lock_tables",
		"drop_sync_trigger", "rename_old_column", "rename_new_column", "commit",
		"drop_sync_function", "drop_old_column"}
	if got := stepNames(p); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("steps\n got %v\nwant %v", got, want)
	}
	if got := stepByName(t, p, "backfill").SQL; !strings.Contains(got,
		`$1::bigint IS NULL OR "id" > $1::bigint ORDER BY "id" LIMIT 5000`) {
		t.Fatalf("backfill does not walk events.id in default batches: %s", got)
	}
	if got := stepByName(t, p, "lock_tables").SQL; got !=
		`LOCK TABLE "public"."events" IN ACCESS EXCLUSIVE MODE` {
		t.Fatalf("lock_tables = %s", got)
	}
	if !strings.Contains(stepByName(t, p, "create_sync_trigger").Note, "fail") {
		t.Fatalf("a non-integer cast does not warn that writes may fail")
	}
}

// Only the contract step drops data, and it is the last step.
func TestColumnSwapOnlyContractIsDestructive(t *testing.T) {
	for _, facts := range []SwapFacts{pkFacts(), textFacts()} {
		req := SwapRequest{Schema: facts.Schema, Table: facts.Table,
			Column: facts.Column.Name, TargetType: "bigint"}
		p := mustSwap(t, facts, req)
		for i, s := range p.Steps {
			last := i == len(p.Steps)-1
			if s.Destructive != last || (last && s.Phase != SwapPhaseContract) {
				t.Fatalf("%s: step %d %s destructive %v phase %s", facts.Table, i, s.Name,
					s.Destructive, s.Phase)
			}
		}
	}
}

// State transitions: phases never go backwards.
func TestColumnSwapPhaseOrder(t *testing.T) {
	order := map[SwapPhase]int{SwapPhasePrepare: 0, SwapPhaseBackfill: 1,
		SwapPhaseVerify: 2, SwapPhaseSwap: 3, SwapPhaseFinish: 4, SwapPhaseContract: 5}
	p := mustSwap(t, pkFacts(), intReq())
	last := -1
	for _, s := range p.Steps {
		rank, ok := order[s.Phase]
		if !ok || rank < last {
			t.Fatalf("%s phase %q after rank %d", s.Name, s.Phase, last)
		}
		last = rank
	}
	if stepByName(t, p, "build_unique_index").RequiresTopLevel != true {
		t.Fatal("CREATE INDEX CONCURRENTLY not marked top-level")
	}
}

func TestColumnSwapScript(t *testing.T) {
	script := mustSwap(t, pkFacts(), intReq()).Script()
	first := strings.Index(script, "SET lock_timeout")
	begin := strings.Index(script, "\nBEGIN;")
	commit := strings.Index(script, "\nCOMMIT;")
	if first < 0 || begin < first || commit < begin {
		t.Fatalf("script order wrong (lock_timeout %d, BEGIN %d, COMMIT %d):\n%s",
			first, begin, commit, script)
	}
	for _, part := range []string{"recommend-only", "-- lock: ACCESS EXCLUSIVE",
		"repeat", "DROP COLUMN \"id__swap_old\";"} {
		if !strings.Contains(script, part) {
			t.Fatalf("script lacks %q:\n%s", part, script)
		}
	}
}

// Identifiers are quoted from their catalog form, embedded quotes doubled.
func TestColumnSwapQuotesIdentifiers(t *testing.T) {
	facts := textFacts()
	facts.Schema, facts.Table, facts.Column.Name = "App", `we"ird`, "Ref"
	p := mustSwap(t, facts, SwapRequest{Schema: "App", Table: `we"ird`, Column: "Ref",
		TargetType: "bigint"})
	if got := stepByName(t, p, "add_column").SQL; got !=
		`ALTER TABLE "App"."we""ird" ADD COLUMN "Ref__swap_new" bigint` {
		t.Fatalf("add_column = %s", got)
	}
}

// Determinism and concurrent access: the planner holds no state, so
// parallel callers get the same plan.
func TestColumnSwapDeterministicUnderConcurrency(t *testing.T) {
	want := mustSwap(t, pkFacts(), intReq()).Script()
	var wg sync.WaitGroup
	errs := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := PlanColumnSwap(context.Background(), pkFacts(), intReq())
			if err != nil || p.Script() != want {
				errs <- "plan differs or failed"
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}

func TestColumnSwapLockTimeoutAndBatchBounds(t *testing.T) {
	req := intReq()
	req.LockTimeout, req.BatchSize = 750*time.Millisecond, 1
	p := mustSwap(t, pkFacts(), req)
	if got := stepByName(t, p, "set_lock_timeout").SQL; got != `SET lock_timeout = '750ms'` {
		t.Fatalf("set_lock_timeout = %s", got)
	}
	req.BatchSize = MaxSwapBatchSize
	mustSwap(t, pkFacts(), req)
	for _, bad := range []SwapRequest{
		{BatchSize: MaxSwapBatchSize + 1}, {BatchSize: -1},
		{LockTimeout: -time.Second}, {LockTimeout: time.Microsecond},
		{LockTimeout: MaxSwapLockTimeout + time.Millisecond},
	} {
		r := intReq()
		r.BatchSize, r.LockTimeout = bad.BatchSize, bad.LockTimeout
		if _, err := PlanColumnSwap(context.Background(), pkFacts(), r); err == nil {
			t.Fatalf("request %+v accepted", bad)
		}
	}
}

func TestColumnSwapCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := PlanColumnSwap(ctx, pkFacts(), intReq())
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("err = %v, want context canceled", err)
	}
}
