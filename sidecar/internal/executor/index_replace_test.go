package executor

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/verify"
)

// The executor's replace action (roadmap 2.3): one approved action that
// creates the wider index CONCURRENTLY, checks it is valid, then drops the
// index it subsumes CONCURRENTLY (a soft drop: its definition is kept and
// re-created on regression). These tests cover the typed contract and the
// pure decisions; index_replace_db_test.go runs it against PostgreSQL.

const (
	rpCreate = "CREATE INDEX CONCURRENTLY orders_status_created ON public.orders " +
		"(status, created_at)"
	rpOldDef = "CREATE INDEX orders_status_idx ON public.orders USING btree (status)"
)

func rpPair() (string, string) {
	return optimizer.IndexReplaceSQL(rpCreate, "public.orders_status_idx"),
		optimizer.IndexReplaceRollbackSQL(rpOldDef, "public.orders_status_created")
}

func rpFinding() analyzer.Finding {
	sql, rollback := rpPair()
	return analyzer.Finding{Category: "tuning_index_replace", ObjectType: "index",
		ObjectIdentifier: "public.orders", Title: "Replace orders_status_idx",
		RecommendedSQL: sql, RollbackSQL: rollback, ActionRisk: "moderate",
		Detail: map[string]any{"table": "public.orders", "queryids": []int64{101},
			"index_replace": map[string]any{"old_index": "public.orders_status_idx",
				"old_index_oid": int64(4242), "old_definition": rpOldDef,
				"new_index": "public.orders_status_created"}}}
}

func TestParseIndexReplace(t *testing.T) {
	sql, rollback := rpPair()
	got, err := ParseIndexReplace(sql, rollback)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := IndexReplace{Table: "public.orders", NewIndex: "public.orders_status_created",
		OldIndex:  "public.orders_status_idx",
		CreateSQL: rpCreate, DropSQL: "DROP INDEX CONCURRENTLY public.orders_status_idx",
		RecreateSQL: "CREATE INDEX CONCURRENTLY orders_status_idx ON public.orders USING " +
			"btree (status)",
		DropNewSQL: "DROP INDEX CONCURRENTLY IF EXISTS public.orders_status_created"}
	if got != want {
		t.Fatalf("plan = %+v\nwant %+v", got, want)
	}
	if !IsIndexReplaceSQL(sql) || IsIndexReplaceSQL(rpCreate) || IsIndexReplaceSQL("") {
		t.Fatal("IsIndexReplaceSQL recognizes exactly a statement pair")
	}
}

func TestParseIndexReplaceRefusesUnsafePairs(t *testing.T) {
	sql, rollback := rpPair()
	pair := func(create, old string) string { return optimizer.IndexReplaceSQL(create, old) }
	undo := func(def, newIndex string) string {
		return optimizer.IndexReplaceRollbackSQL(def, newIndex)
	}
	cases := map[string][2]string{
		"empty":       {"", ""},
		"one create":  {rpCreate, rollback},
		"no rollback": {sql, ""},
		"unique new index adds a uniqueness rule": {pair("CREATE UNIQUE INDEX "+
			"CONCURRENTLY orders_status_created ON public.orders (status, created_at)",
			"public.orders_status_idx"), rollback},
		"if not exists": {pair("CREATE INDEX CONCURRENTLY IF NOT EXISTS "+
			"orders_status_created ON public.orders (status, created_at)",
			"public.orders_status_idx"), rollback},
		"unnamed new index": {pair("CREATE INDEX CONCURRENTLY ON public.orders "+
			"(status, created_at)", "public.orders_status_idx"), rollback},
		"unqualified table": {pair("CREATE INDEX CONCURRENTLY orders_status_created ON "+
			"orders (status, created_at)", "public.orders_status_idx"), rollback},
		"unqualified old index": {pair(rpCreate, "orders_status_idx"), rollback},
		"old index in another schema": {pair(rpCreate, "sales.orders_status_idx"),
			undo(rpOldDef, "public.orders_status_created")},
		"drops the new index": {pair(rpCreate, "public.orders_status_created"),
			undo("CREATE INDEX orders_status_created ON public.orders USING btree (status)",
				"public.orders_status_created")},
		"undo re-creates another index": {sql, undo("CREATE INDEX other_idx ON "+
			"public.orders USING btree (status)", "public.orders_status_created")},
		"undo re-creates on another table": {sql, undo("CREATE INDEX orders_status_idx "+
			"ON public.items USING btree (status)", "public.orders_status_created")},
		"undo drops another index": {sql, undo(rpOldDef, "public.other_idx")},
		"statement smuggled into the create": {
			"CREATE INDEX CONCURRENTLY orders_status_created ON public.orders (status); " +
				"DROP TABLE public.orders;\nDROP INDEX CONCURRENTLY public.orders_status_idx;",
			rollback},
	}
	for name, c := range cases {
		if plan, err := ParseIndexReplace(c[0], c[1]); err == nil {
			t.Fatalf("%s: accepted %+v", name, plan)
		} else if !errors.Is(err, ErrReplaceInvalid) {
			t.Fatalf("%s: error %v is not ErrReplaceInvalid", name, err)
		}
	}
}

func TestIndexReplaceSubsumption(t *testing.T) {
	plan, err := ParseIndexReplace(rpPair())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := plan.checkSubsumes(rpOldDef); err != nil {
		t.Fatalf("(status, created_at) subsumes (status): %v", err)
	}
	other := "CREATE INDEX orders_status_idx ON public.orders USING btree (created_at)"
	if err := plan.checkSubsumes(other); !errors.Is(err, ErrReplaceNotSubsumed) {
		t.Fatalf("(status, created_at) does not subsume (created_at): %v", err)
	}
	if err := plan.checkSubsumes(""); !errors.Is(err, ErrReplaceNotSubsumed) {
		t.Fatalf("an unknown definition is never subsumed: %v", err)
	}
}

func TestIndexReplaceClassification(t *testing.T) {
	sql, _ := rpPair()
	if got := actionTypeForProposalSQL(sql); got != ActionTypeReplaceIndex {
		t.Fatalf("contract type = %q", got)
	}
	if got := categorizeAction(sql); got != ActionTypeReplaceIndex {
		t.Fatalf("action_log label = %q", got)
	}
	if got := verificationClass(sql); got != verify.ClassIndexReplace {
		t.Fatalf("verification class = %q", got)
	}
	if got := changeClassForActionType(ActionTypeReplaceIndex); got !=
		string(policy.ChangeIndex) {
		t.Fatalf("change class = %q", got)
	}
	if got := dropKindForActionType(ActionTypeReplaceIndex); got != policy.DropDerivable {
		t.Fatalf("drop kind = %q: the old index is rebuilt from its definition", got)
	}
	if !partitionScoped(sql) {
		t.Fatal("an index replacement moves the plans of a whole partition tree")
	}
	// A plain create and drop keep their own types.
	if actionTypeForProposalSQL(rpCreate) != "create_index_concurrently" ||
		categorizeAction("DROP INDEX CONCURRENTLY public.x") != "drop_index" {
		t.Fatal("single statements are classified as before")
	}
}

func TestIndexReplaceContractNeedsAnApproval(t *testing.T) {
	c, ok := ContractForActionType(ActionTypeReplaceIndex)
	if !ok || c.Validate() != nil {
		t.Fatalf("contract = %+v ok=%v", c, ok)
	}
	if c.RollbackClass != "reversible" || c.BaseRiskTier != "moderate" {
		t.Fatalf("rollback class %q tier %q", c.RollbackClass, c.BaseRiskTier)
	}
	plan := strings.Join(c.ExecutionPlan, " | ")
	if !strings.Contains(plan, "CREATE INDEX CONCURRENTLY") ||
		!strings.Contains(plan, "DROP INDEX CONCURRENTLY") {
		t.Fatalf("execution plan names both steps: %q", plan)
	}
	pc := policyContract(c)
	if len(pc.Guardrails) != 1 || pc.Guardrails[0] != policy.GuardrailApprovalRequired {
		t.Fatalf("a replace never runs unattended: guardrails %v", pc.Guardrails)
	}
	if pc.DropKind != policy.DropDerivable || pc.ActionType != ActionTypeReplaceIndex {
		t.Fatalf("policy contract = %+v", pc)
	}
}

func TestIndexReplaceRequests(t *testing.T) {
	sql, _ := rpPair()
	targets := []string{"public.orders", "public.orders_status_idx"}
	req, contract := operatorRequest(sql, 7, nil)
	if req.SQL != rpCreate || req.Contract == nil ||
		req.Contract.ActionType != ActionTypeReplaceIndex ||
		contract.ActionType != ActionTypeReplaceIndex || !req.OperatorApproved ||
		!reflect.DeepEqual(req.TargetObjs, targets) {
		t.Fatalf("operator request = %+v", req)
	}
	if got := operatorLeaseTargets(sql); !reflect.DeepEqual(got, targets) {
		t.Fatalf("the lease covers the table and the old index: %v", got)
	}
	freq := findingRequest(rpFinding(), false)
	if freq.SQL != rpCreate || freq.Contract == nil ||
		freq.Contract.ActionType != ActionTypeReplaceIndex ||
		!reflect.DeepEqual(freq.TargetObjs, targets) ||
		freq.Feature != string(policy.ChangeIndex) {
		t.Fatalf("finding request = %+v", freq)
	}
	if c, ok := contractForFinding(rpFinding()); !ok || c.ActionType != ActionTypeReplaceIndex {
		t.Fatalf("finding contract = %+v %v", c, ok)
	}
	e := New(nil, config.DefaultConfig(), time.Time{}, nopLog)
	if err := e.findingRefusal(rpFinding()); !errors.Is(err, ErrReplaceApprovalRequired) {
		t.Fatalf("the cycle never runs a replace itself: %v", err)
	}
	queued := store.QueuedAction{ActionType: ActionTypeReplaceIndex, ProposedSQL: sql}
	if c, ok := ContractForQueuedAction(queued); !ok ||
		c.ActionType != ActionTypeReplaceIndex {
		t.Fatalf("queued contract = %+v", c)
	}
	queued.ActionType = ""
	if c, ok := ContractForQueuedAction(queued); !ok ||
		c.ActionType != ActionTypeReplaceIndex {
		t.Fatalf("a legacy row without a type is typed from its SQL: %+v", c)
	}
}

func TestIndexReplaceIsQueuedEvenWhenTrustIsAutonomous(t *testing.T) {
	cfg := &config.Config{}
	cfg.Trust.Level = "autonomous"
	cfg.Trust.Tier3Safe, cfg.Trust.Tier3Moderate = true, true
	exec := New(nil, cfg, time.Now().Add(-90*24*time.Hour), nopLog)
	exec.SetExecutionMode("auto")
	withTestStandingGateAt(exec, time.Now())
	decision := exec.evaluateFindingPolicy(t.Context(), rpFinding(), false)
	if decision.Decision != PolicyDecisionQueueApproval {
		t.Fatalf("decision = %+v, want queue_approval", decision)
	}
}

func TestIndexReplaceApprovalReadiness(t *testing.T) {
	sql, rollback := rpPair()
	cfg := &config.Config{}
	cfg.Trust.Level = "autonomous"
	cfg.Trust.Tier3Moderate = true
	exec := New(nil, cfg, time.Now().Add(-40*24*time.Hour), nopLog)
	exec.SetExecutionMode("approval")
	now := time.Now().UTC()
	withTestStandingGateAt(exec, now)
	got := exec.ApprovalReadiness(store.QueuedAction{ActionType: ActionTypeReplaceIndex,
		ActionRisk: "moderate", Status: "pending", ProposedSQL: sql, RollbackSQL: rollback,
		ExpiresAt: now.Add(time.Hour)}, now)
	if !got.Eligible || got.Policy.Decision != PolicyDecisionExecute {
		t.Fatalf("an operator may approve the pair: %+v", got)
	}
}

func TestResumeStepFor(t *testing.T) {
	type cat = replaceCatalog
	both := cat{NewExists: true, NewValid: true, OldExists: true, OldValid: true}
	cases := []struct {
		state string
		c     replaceCatalog
		want  replaceStep
	}{
		{replaceCreating, cat{OldExists: true, OldValid: true}, stepFailCreate},
		{replaceCreating, cat{NewExists: true, OldExists: true, OldValid: true},
			stepDropRemnant},
		{replaceCreating, both, stepDropOld},
		{replaceCreated, both, stepDropOld},
		{replaceDropping, both, stepDropOld},
		{replaceDropping, cat{NewExists: true, NewValid: true, OldExists: true}, stepDropOld},
		{replaceDropping, cat{NewExists: true, NewValid: true}, stepComplete},
		{replaceCreated, cat{OldExists: true, OldValid: true}, stepRestore},
		{replaceDropping, cat{NewExists: true, OldExists: true, OldValid: true},
			stepRestore},
		{replaceDropping, cat{}, stepRestore},
		{replaceRollbackRecreating, cat{NewExists: true, NewValid: true}, stepRecreateOld},
		{replaceRollbackRecreating, cat{NewExists: true, NewValid: true, OldExists: true},
			stepRecreateOld},
		{replaceRollbackRecreating, both, stepDropNew},
		{replaceRollbackDropping, both, stepDropNew},
		{replaceRollbackDropping, cat{OldExists: true, OldValid: true}, stepRolledBack},
		{replaceRollbackDropping, cat{}, stepRecreateOld},
		{replaceOldRestoring, cat{NewExists: true, NewValid: true}, stepRestoreOld},
		{replaceOldRestoring, both, stepOldRestored},
		{replaceCompleted, both, stepNone},
		{replaceCreateFailed, cat{}, stepNone},
		{replaceDropFailed, both, stepNone},
		{replaceRolledBack, cat{OldExists: true, OldValid: true}, stepNone},
		{replaceRollbackFailed, cat{}, stepNone},
		{replaceOldRestored, both, stepNone},
		{"bogus", both, stepNone},
		{"", cat{}, stepNone},
	}
	for _, c := range cases {
		if got := resumeStepFor(c.state, c.c); got != c.want {
			t.Fatalf("resumeStepFor(%q, %+v) = %q, want %q", c.state, c.c, got, c.want)
		}
	}
}

// No concurrent access tests here: the parser, classification, resume and
// verdict decisions are pure functions (the DB tests cover the lease).

func TestDecideReplace(t *testing.T) {
	improved, neutral := cmp(verify.OutcomeImproved), cmp(verify.OutcomeNeutral)
	regressed, thin := cmp(verify.OutcomeRegressed), cmp(verify.OutcomeInsufficient)
	j := func(in replaceJudgeInput) replaceJudgeInput { in.Phase = replacePhaseJudging; return in }
	w := func(in replaceJudgeInput) replaceJudgeInput { in.Phase = replacePhaseWatch; return in }
	cases := []struct {
		name    string
		in      replaceJudgeInput
		action  replaceAction
		verdict string
	}{
		{"targets regressed", j(replaceJudgeInput{Targets: regressed, Guarded: improved}),
			actRollback, verify.OutcomeRegressed},
		{"old index users regressed", j(replaceJudgeInput{Targets: improved,
			Guarded: regressed, PastMin: true}), actRollback, verify.OutcomeRegressed},
		{"a hint names the old index", j(replaceJudgeInput{Targets: improved,
			HintNamesOld: true}), actRollback, verify.OutcomeRegressed},
		{"before the first window", j(replaceJudgeInput{Targets: improved}), actWait, ""},
		{"improved", j(replaceJudgeInput{Targets: improved, Guarded: neutral,
			PastMin: true}), actKeep, verify.OutcomeImproved},
		{"improved without guarded queries", j(replaceJudgeInput{Targets: improved,
			PastMin: true}), actKeep, verify.OutcomeImproved},
		{"no gain", j(replaceJudgeInput{Targets: neutral, PastMin: true}), actRollback,
			verify.OutcomeNeutral},
		{"thin evidence waits", j(replaceJudgeInput{Targets: thin, PastMin: true}),
			actWait, ""},
		{"thin evidence at the cap", j(replaceJudgeInput{Targets: thin, PastMin: true,
			AtCap: true}), actRollback, verify.OutcomeUnverifiable},
		{"no targets at the cap", j(replaceJudgeInput{PastMin: true, AtCap: true}),
			actRollback, verify.OutcomeUnverifiable},
		{"watch: old index users regressed", w(replaceJudgeInput{Guarded: regressed}),
			actRestoreOld, verify.OutcomeRegressed},
		{"watch: a hint names the old index", w(replaceJudgeInput{HintNamesOld: true}),
			actRestoreOld, verify.OutcomeRegressed},
		{"watch: targets regressing no longer roll back", w(replaceJudgeInput{
			Targets: regressed, Guarded: neutral}), actWait, ""},
		{"watch: holding", w(replaceJudgeInput{Guarded: neutral}), actWait, ""},
		{"watch: drop window over", w(replaceJudgeInput{Guarded: neutral,
			PastDropWindow: true}), actDone, ""},
		{"unknown phase", replaceJudgeInput{Phase: "x", Targets: regressed}, actDone, ""},
	}
	for _, c := range cases {
		got := decideReplace(c.in)
		if got.Action != c.action || got.Verdict != c.verdict {
			t.Fatalf("%s: %+v, want %s/%s", c.name, got, c.action, c.verdict)
		}
		if got.Action != actWait && got.Action != actDone && got.Reason == "" {
			t.Fatalf("%s: a decision that changes something says why: %+v", c.name, got)
		}
	}
}

func TestSupportsForeignKey(t *testing.T) {
	cases := []struct {
		name    string
		lead    []string
		partial bool
		fk      []string
		want    bool
	}{
		{"leading column", []string{"parent_id", "b"}, false, []string{"parent_id"}, true},
		{"leading set in another order", []string{"b", "a", "c"}, false,
			[]string{"a", "b"}, true},
		{"second column only", []string{"b", "parent_id"}, false, []string{"parent_id"},
			false},
		{"partial index", []string{"parent_id"}, true, []string{"parent_id"}, false},
		{"too few columns", []string{"a"}, false, []string{"a", "b"}, false},
		{"empty foreign key", []string{"a"}, false, nil, false},
		{"no columns", nil, false, []string{"a"}, false},
	}
	for _, c := range cases {
		if got := supportsForeignKey(c.lead, c.partial, c.fk); got != c.want {
			t.Fatalf("%s: supportsForeignKey = %v, want %v", c.name, got, c.want)
		}
	}
}
