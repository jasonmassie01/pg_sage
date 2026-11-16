package auditchain_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// No concurrent access tests: canonicalization is a pure function of one
// row and the session's settings; the writer ordering is covered by
// concurrency_db_test.go.

// goldenRow is what a chain records for one fixed row.
type goldenRow struct {
	RowID      int64  `json:"row_id"`
	SealedText string `json:"sealed_text"`
	SealedHash string `json:"sealed_hash"`
	State      string `json:"state"`
	LinkHash   string `json:"link_hash"`
}

type goldenFile struct {
	Comment string               `json:"comment"`
	Chains  map[string]goldenRow `json:"chains"`
}

type goldenFixture struct {
	id  int64
	sql string
}

// goldenFixtures is one fixed row per built-in chain: fixed ids, ISO
// timestamps with an offset (parsed the same under every DateStyle and
// TimeZone), unicode, quotes and backslashes, nested jsonb with unsorted
// keys, a numeric with trailing zeros, NULLs, a text[] and a uuid.
var goldenFixtures = map[string]goldenFixture{
	"action_log": {1001, `INSERT INTO sage.action_log (id, executed_at, action_type,
		finding_id, sql_executed, rollback_sql, before_state, approved_by, approved_at,
		decision_id, database_id, outcome, rollback_reason, measured_at, after_state,
		verification_id, justification, toil_minutes_saved, toil_model_version)
		VALUES (1001, '2026-03-04 05:06:07.123456+00', 'create_index', 42,
		'CREATE INDEX CONCURRENTLY "idx_ünï" ON public.t (c) -- 東京 ✓',
		'DROP INDEX CONCURRENTLY "idx_ünï"',
		'{"z": {"b": [1, 2.50, "x"], "a": null}, "a": "é\"q"}', 7,
		'2026-03-04 05:00:00+00', NULL, 3, 'success', NULL, '2026-03-04 06:00:00.5+00',
		'{"rows": 1000, "nested": {"k": [true, false], "f": 0.1}}', NULL,
		E'Ünicode ✓ "quoted" \\ backslash\ttab', 12.50, 2)`},
	"auth_audit": {2002, `INSERT INTO sage.auth_audit (id, event, actor_user_id,
		target_user_id, detail, created_at, source_ip) VALUES (2002, 'login_failed', 0, 7,
		'{"reason": "bad password", "client": {"ua": "Mozilla/5.0 ü", "v": [1, 2]}}',
		'2026-03-04 07:08:09.000001+00', '2001:db8::1')`},
	"config_audit": {3003, `INSERT INTO sage.config_audit (id, key, old_value, new_value,
		database_id, changed_by, changed_at, changed_by_actor) VALUES (3003, 'trust.level',
		'observation', 'advisory ✓', 3, 7, '2026-03-04 08:09:10.999999+00', 'admin:ünï')`},
	"guard_query_audit": {4004, `INSERT INTO sage.guard_query_audit (id, database_id,
		principal_id, task_id, envelope_hash, verdict, reason, row_count, classes, at)
		VALUES (4004, '0b1e2c3d-4e5f-6071-8293-a4b5c6d7e8f9', 'agent:ünï', 'task-東京',
		'sha256:0f1e', 'blocked', 'write_detected', 0, ARRAY['ddl', 'pii ü'],
		'2026-03-04 09:10:11.5+00')`},
	"pgaudit_events": {5005, `INSERT INTO sage.guard_pgaudit_events (id, database_name,
		logged_at, session_id, pid, db_user, application, principal_id, action_id,
		audit_type, statement_id, substatement_id, class, command, object_type, object_name,
		statement, correlated_by) VALUES (5005, 'app_db', '2026-03-04 10:11:12.25+00',
		'65f1.2a', 4242, 'sage_agent', 'pg_sage ✓', 'agent:ünï', NULL, 'SESSION', 3, 1,
		'DDL', 'CREATE INDEX', 'INDEX', 'public.idx_ünï', 'CREATE INDEX "idx_ünï" ON t (c)',
		'application_name')`},
	"fact_events": {6006, `INSERT INTO sage.fact_events (id, fact_id, at, actor, verb,
		from_status, to_status, fact_hash, note) VALUES (6006, 42,
		'2026-03-04 11:12:13.654321+00', 'admin:ünï', 'confirmed', 'proposed', 'confirmed',
		'0123456789abcdef0123456789abcdef', E'Ünicode ✓ "quoted" \\ backslash\ttab 東京')`},
	// audit-11's decision ledger and policy approval chains.
	"decision": {7007, `INSERT INTO sage.decision (id, database_id, feature, intent,
		target_objects, policy_version, policy_doc_sha256, verdict, risk_tier, reason,
		guardrails, off_window_ok, deadline_kind, deadline_hard_at, evidence, evidence_id,
		fingerprint, principal_id, task_id, artifact_hash, candidate_key, created_at,
		resolved_at) VALUES (7007, 41, 'index', 'create_index',
		'["public.ünï", "public.orders"]', 3,
		'0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef', 'queue_approval',
		'high', E'blast radius ✓ "quoted" \\ tab\t', '["budget", "window"]', true, 'soft',
		'2026-03-04 12:13:14.5+00', '{"z": {"b": [1, 2.50], "a": null}, "a": "é"}',
		'ev-golden-東京', 'fp_0123456789abcdef', 'agp_ünï', 'task-7', 'sha256:0f1e',
		'ck_0123456789abcdef', '2026-03-04 12:00:00.000001+00', NULL)`},
	"policy_approvals": {8008, `WITH p AS (INSERT INTO sage.policy (id, database_id, version,
		doc, status, proposed_by) VALUES (8000, 41, 9, '{"b": 1, "a": [2.50]}', 'proposed',
		'agent:ünï') RETURNING id)
		INSERT INTO sage.policy_approvals (id, policy_id, approver, approver_user_id, reason,
		single_operator, review_status, decided_at) OVERRIDING SYSTEM VALUE
		SELECT 8008, p.id, 'ünï@example.com', 7, E'Ünicode ✓ "quoted" \\ backslash\ttab',
		true, 'pending', '2026-03-04 13:14:15.123456+00' FROM p`},
	// plan-01b's plan event and model record chains.
	"plan_events": {9009, `INSERT INTO sage.plan_event (id, plan_id, step_id, kind,
		plan_hash, actor, detail, created_at) VALUES (9009, 41, NULL, 'approved',
		'0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef', 'user:ünï',
		'{"z": {"b": [1, 2.50], "a": null}, "approval_id": 7, "q": "é\"q"}',
		'2026-03-04 14:15:16.000001+00')`},
	"plan_model_records": {10010, `INSERT INTO sage.plan_model_record (id, plan_id,
		pack_hash, model, prompt_version, raw_output, raw_output_sha256, created_at)
		VALUES (10010, 41,
		'abababababababababababababababababababababababababababababababab', 'model-ünï',
		'plan-v1', E'` + "```json\\n" + `{"goal": "東京 ✓", "steps": [1, 2.50]}\n` + "```" + `',
		'fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210',
		'2026-03-04 15:16:17.5+00')`},
}

var (
	setChatham   = "SET LOCAL TimeZone = 'Pacific/Chatham'"
	setDateStyle = "SET LOCAL DateStyle = 'SQL, DMY'"
	setFloat     = "SET LOCAL extra_float_digits = 0"
	setInterval  = "SET LOCAL IntervalStyle = 'postgres_verbose'"
	setBytea     = "SET LOCAL bytea_output = 'escape'"
)

// sessionProfiles are the settings that change how PostgreSQL renders
// values; none may move a hash.
var sessionProfiles = []struct {
	name string
	sets []string
}{
	{"default", nil},
	{"timezone_chatham", []string{setChatham}},
	{"datestyle_sql_dmy", []string{setDateStyle}},
	{"extra_float_digits_0", []string{setFloat}},
	{"intervalstyle_verbose", []string{setInterval}},
	{"bytea_escape", []string{setBytea}},
	{"all", []string{setChatham, setDateStyle, setFloat, setInterval, setBytea}},
}

func loadGolden(t *testing.T) map[string]goldenRow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "golden_hashes.json"))
	if err != nil {
		t.Fatalf("read golden hashes: %v", err)
	}
	var f goldenFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse testdata/golden_hashes.json: %v", err)
	}
	return f.Chains
}

// tableExists reports whether spec's table exists in q's database.
func tableExists(ctx context.Context, q pgx.Tx, spec auditchain.Spec) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", spec.Table).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("look up %s: %w", spec.Table, err)
	}
	return ok, nil
}

// absentReason is why spec is skipped: empty when its table exists, a
// reason when an optional table is absent, an error when a required one is.
func absentReason(ctx context.Context, q pgx.Tx, spec auditchain.Spec) (string, error) {
	ok, err := tableExists(ctx, q, spec)
	switch {
	case err != nil:
		return "", err
	case ok:
		return "", nil
	case spec.Optional:
		return fmt.Sprintf("chain %s skipped: optional table %s is absent", spec.Chain,
			spec.Table), nil
	}
	return "", fmt.Errorf("chain %s: required table %s is absent", spec.Chain, spec.Table)
}

// insertGoldenFixtures writes every present chain's fixed row in tx.
func insertGoldenFixtures(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	for _, spec := range auditchain.Specs() {
		reason, err := absentReason(ctx, tx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if reason != "" {
			t.Log(reason)
			continue
		}
		if _, err := tx.Exec(ctx, goldenFixtures[spec.Chain].sql); err != nil {
			t.Fatalf("insert %s fixture: %v", spec.Chain, err)
		}
	}
}

// readGolden recomputes spec's fixture row's canonical text in q's session
// and reads its insert link, which must be the chain's first.
func readGolden(ctx context.Context, q pgx.Tx, spec auditchain.Spec) (goldenRow, error) {
	fx, ok := goldenFixtures[spec.Chain]
	if !ok {
		return goldenRow{}, fmt.Errorf("chain %s has no golden fixture", spec.Chain)
	}
	got := goldenRow{RowID: fx.id}
	var rowHash *string
	sql := "SELECT " + auditchain.SealedSQLForTest(spec, "r") + ", " +
		auditchain.SealedHashSQLForTest(spec, "r") + ", " +
		auditchain.StateSQLForTest(spec, "r") + ", r.chain_hash FROM " + spec.Table +
		" r WHERE r.id = $1"
	if err := q.QueryRow(ctx, sql, fx.id).Scan(&got.SealedText, &got.SealedHash,
		&got.State, &rowHash); err != nil {
		return goldenRow{}, fmt.Errorf("canonical text of %s row %d: %w", spec.Chain, fx.id, err)
	}
	links, err := auditchain.LinksForRow(ctx, q, spec.Chain, fx.id)
	if err != nil {
		return goldenRow{}, err
	}
	if len(links) != 1 {
		return goldenRow{}, fmt.Errorf("%s row %d has %d links, want 1", spec.Chain, fx.id,
			len(links))
	}
	l := links[0]
	if l.Seq != 1 || l.Op != "I" || l.V != spec.Version || l.PrevHash != genesis ||
		l.SealedHash != got.SealedHash || l.State != got.State {
		return goldenRow{}, fmt.Errorf("%s insert link %+v does not match the row's "+
			"canonical text (sealed %s, state %q) at seq 1", spec.Chain, l, got.SealedHash,
			got.State)
	}
	if rowHash == nil || *rowHash != l.Hash {
		return goldenRow{}, fmt.Errorf("%s row %d chain_hash %v, want its link %s",
			spec.Chain, fx.id, rowHash, l.Hash)
	}
	got.LinkHash = l.Hash
	return got, nil
}

// checkGolden compares got with the pinned row and with the documented
// formulas recomputed in Go.
func checkGolden(t *testing.T, profile string, spec auditchain.Spec, got, want goldenRow) {
	t.Helper()
	pretty, _ := json.MarshalIndent(map[string]goldenRow{spec.Chain: got}, "    ", "  ")
	if got != want {
		t.Errorf("%s under %s: canonical output moved.\n got: %s\nwant: %+v\n"+
			"A sealed or state column's type, a canonical expression, or PostgreSQL's "+
			"jsonb text output changed: every historical row would now verify as "+
			"edited. Bump Spec.Version and add a registry entry; never edit the pins.",
			spec.Chain, profile, pretty, want)
	}
	sum := sha256.Sum256([]byte(got.SealedText))
	if hex.EncodeToString(sum[:]) != got.SealedHash {
		t.Errorf("%s under %s: sealed_hash is not sha256 of the sealed text", spec.Chain,
			profile)
	}
	link := auditchain.Link{Seq: 1, RowID: got.RowID, Op: "I", V: spec.Version,
		SealedHash: got.SealedHash, State: got.State, PrevHash: genesis}
	if auditchain.LinkHash(link) != got.LinkHash {
		t.Errorf("%s under %s: the trigger's link hash differs from LinkHash", spec.Chain,
			profile)
	}
}

// checkProfile applies a profile's settings in tx, then checks every
// present chain against the pins and verifies it.
func checkProfile(t *testing.T, ctx context.Context, tx pgx.Tx, sets []string,
	profile string, golden map[string]goldenRow) {
	t.Helper()
	for _, spec := range auditchain.Specs() {
		reason, err := absentReason(ctx, tx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if reason != "" {
			t.Log(reason)
			continue
		}
		want, ok := golden[spec.Chain]
		if !ok {
			t.Fatalf("testdata/golden_hashes.json has no %s entry", spec.Chain)
		}
		got, err := readGolden(ctx, tx, spec)
		if err != nil {
			t.Fatalf("%s under %s: %v", spec.Chain, profile, err)
		}
		checkGolden(t, profile, spec, got, want)
		rep, err := auditchain.Verify(ctx, tx, spec, auditchain.Window{})
		if err != nil {
			t.Fatalf("verify %s under %s: %v", spec.Chain, profile, err)
		}
		if !rep.OK() || rep.Links != 1 || rep.RowsChecked != 1 {
			t.Errorf("verify %s under %s (%s): %+v", spec.Chain, profile,
				strings.Join(sets, "; "), rep)
		}
	}
}

func beginProfile(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	sets []string) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	for _, s := range sets {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return tx
}

// Written under every session setting, each chain's fixed row records the
// pinned canonical text, sealed hash, state and link hash, and verifies.
// Each profile writes in a transaction it rolls back, so every run starts
// from an empty chain.
func TestGoldenHashesWrittenUnderEverySessionSetting(t *testing.T) {
	pool := freshDB(t, "chain_golden_write")
	golden := loadGolden(t)
	ctx := context.Background()
	for _, p := range sessionProfiles {
		t.Run(p.name, func(t *testing.T) {
			tx := beginProfile(t, ctx, pool, p.sets)
			insertGoldenFixtures(t, ctx, tx)
			if _, err := tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
				t.Fatalf("fire the chain triggers: %v", err)
			}
			checkProfile(t, ctx, tx, p.sets, p.name, golden)
		})
	}
}

// Rows written under the default settings verify, and recompute to the
// pins, when the verifier's session uses any other setting.
func TestGoldenHashesReadUnderEverySessionSetting(t *testing.T) {
	pool := freshDB(t, "chain_golden_read")
	golden := loadGolden(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	insertGoldenFixtures(t, ctx, tx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixtures: %v", err)
	}
	for _, p := range sessionProfiles {
		t.Run(p.name, func(t *testing.T) {
			checkProfile(t, ctx, beginProfile(t, ctx, pool, p.sets), p.sets, p.name, golden)
		})
	}
}

// An optional chain whose table is absent is skipped with a reason; a
// required one whose table is absent is an error, never a silent pass.
func TestGoldenSkipsOnlyAbsentOptionalTables(t *testing.T) {
	pool := freshDB(t, "chain_golden_absent")
	exec(t, pool, "DROP TABLE sage.guard_query_audit CASCADE")
	exec(t, pool, "DROP TABLE sage.guard_pgaudit_events CASCADE")
	ctx := context.Background()
	tx := beginProfile(t, ctx, pool, nil)
	reason, err := absentReason(ctx, tx, auditchain.GuardQueryAudit)
	if err != nil || !strings.Contains(reason, "guard_query_audit") ||
		!strings.Contains(reason, "absent") {
		t.Fatalf("absent optional table: reason %q err %v, want a named skip", reason, err)
	}
	t.Log(reason)
	reason, err = absentReason(ctx, tx, auditchain.PGAuditEvents)
	if err == nil || reason != "" || !strings.Contains(err.Error(), "guard_pgaudit_events") {
		t.Fatalf("absent required table: reason %q err %v, want an error", reason, err)
	}
	if reason, err := absentReason(ctx, tx, auditchain.ActionLog); err != nil || reason != "" {
		t.Fatalf("present table: reason %q err %v, want neither", reason, err)
	}
}
