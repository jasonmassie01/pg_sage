package evidence

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// policy-03: policy.json selected sage.policy rows by a decision column
// that nothing wrote, so every pack shipped an empty policy.json. The
// pack now fetches the policy each decision names by (database scope,
// version) from the policy database (the control database in fleet mode)
// and the manifest records whether its document hash matches the one the
// decision recorded.

const packPolicyDoc = `{"allowed_change_classes": ["index"], "maintenance_windows": ["always"]}`

// seedPolicy stores policy version 3 of database scope 41 and returns the
// hash of its stored document, computed the way the gate computes it.
func seedPolicy(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var hash string
	err := pool.QueryRow(context.Background(), `INSERT INTO sage.policy (database_id,
		version, profile, doc, status, proposed_by, ratified_by) VALUES (41, 3,
		'unattended', $1::jsonb, 'active', 'test', 'test')
		RETURNING encode(sha256(convert_to(doc::text, 'UTF8')), 'hex')`,
		packPolicyDoc).Scan(&hash)
	if err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	return hash
}

// seedExecutedDecision records an executed action whose decision names
// policy v3 of database 41 with hash, and returns the action id.
func seedExecutedDecision(t *testing.T, pool *pgxpool.Pool, hash *string) int64 {
	t.Helper()
	ctx := context.Background()
	var decision, action int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.decision (database_id, feature, intent,
		verdict, risk_tier, reason, evidence_id, policy_version, policy_doc_sha256)
		VALUES (41, 'index', 'index', 'execute', 'safe', 'authorized', 'ev-p03', 3, $1)
		RETURNING id`, hash).Scan(&decision)
	if err != nil {
		t.Fatalf("seed decision: %v", err)
	}
	err = pool.QueryRow(ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
		outcome, decision_id) VALUES ('create_index', 'CREATE INDEX CONCURRENTLY i ON t (a)',
		'success', $1) RETURNING id`, decision).Scan(&action)
	if err != nil {
		t.Fatalf("seed action: %v", err)
	}
	return action
}

type packPolicyRow struct {
	ID        int64  `json:"id"`
	Version   int    `json:"version"`
	Database  *int64 `json:"database_id"`
	DocSHA256 string `json:"doc_sha256"`
}

func buildPolicyPack(
	t *testing.T, ledgerDB, policyDB *pgxpool.Pool, action int64,
) (Manifest, []packPolicyRow, map[string][]byte) {
	t.Helper()
	pack, err := Build(context.Background(), Sources{DB: ledgerDB, Control: policyDB},
		Scope{ActionID: action}, nil, time.Now())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var buf bytes.Buffer
	if err := pack.WriteTarGz(&buf); err != nil {
		t.Fatalf("write: %v", err)
	}
	files := readPack(t, buf.Bytes())
	var m Manifest
	if err := json.Unmarshal(files["manifest.json"], &m); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var rows []packPolicyRow
	if err := json.Unmarshal(files["policy.json"], &rows); err != nil {
		t.Fatalf("policy.json %s: %v", files["policy.json"], err)
	}
	if _, err := Verify(bytes.NewReader(buf.Bytes()), nil); err != nil {
		t.Fatalf("the pack failed verification: %v", err)
	}
	return m, rows, files
}

func TestActionPackHoldsThePolicyItsDecisionNames(t *testing.T) {
	pool := freshDB(t, "evidence_policy")
	hash := seedPolicy(t, pool)
	action := seedExecutedDecision(t, pool, &hash)
	m, rows, _ := buildPolicyPack(t, pool, pool, action)
	if len(rows) != 1 || rows[0].Version != 3 || rows[0].Database == nil ||
		*rows[0].Database != 41 || rows[0].DocSHA256 != hash {
		t.Fatalf("policy.json = %+v, want policy v3 of database 41 hashed %s", rows, hash)
	}
	if m.PolicyHashMatch == nil || !*m.PolicyHashMatch {
		t.Fatalf("manifest policy_hash_match = %v, want true", m.PolicyHashMatch)
	}
}

// Build (one database) keeps working: the policy is read where the ledger is.
func TestBuildReadsThePolicyFromTheLedgerDatabase(t *testing.T) {
	pool := freshDB(t, "evidence_policy_build")
	hash := seedPolicy(t, pool)
	action := seedExecutedDecision(t, pool, &hash)
	pack, err := Build(context.Background(), Sources{DB: pool}, Scope{ActionID: action},
		nil, time.Now())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if pack.Manifest.PolicyHashMatch == nil || !*pack.Manifest.PolicyHashMatch {
		t.Fatalf("policy_hash_match = %v, want true", pack.Manifest.PolicyHashMatch)
	}
}

// A policy document edited after the decision no longer matches its hash.
func TestEditedPolicyDocumentFailsTheHashMatch(t *testing.T) {
	pool := freshDB(t, "evidence_policy_edit")
	hash := seedPolicy(t, pool)
	action := seedExecutedDecision(t, pool, &hash)
	mustExec(t, pool, `UPDATE sage.policy SET doc = doc || '{"refusal_set": []}'::jsonb
		WHERE database_id = 41 AND version = 3`)
	m, rows, _ := buildPolicyPack(t, pool, pool, action)
	if len(rows) != 1 || rows[0].DocSHA256 == hash {
		t.Fatalf("policy.json = %+v, want the edited row with a new hash", rows)
	}
	if m.PolicyHashMatch == nil || *m.PolicyHashMatch {
		t.Fatalf("manifest policy_hash_match = %v, want false", m.PolicyHashMatch)
	}
}

// A decision naming a version the policy database does not hold fails the
// match: the pack cannot show the policy the decision was made under.
func TestMissingPolicyVersionFailsTheHashMatch(t *testing.T) {
	pool := freshDB(t, "evidence_policy_missing")
	hash := seedPolicy(t, pool)
	action := seedExecutedDecision(t, pool, &hash)
	mustExec(t, pool, `DELETE FROM sage.policy WHERE database_id = 41`)
	m, rows, _ := buildPolicyPack(t, pool, pool, action)
	if len(rows) != 0 {
		t.Fatalf("policy.json = %+v, want empty", rows)
	}
	if m.PolicyHashMatch == nil || *m.PolicyHashMatch {
		t.Fatalf("manifest policy_hash_match = %v, want false", m.PolicyHashMatch)
	}
}

// Fleet: the decision is in the monitored database, the policy in the
// control database. The pack reads each from its own.
func TestFleetPackReadsThePolicyFromTheControlDatabase(t *testing.T) {
	monitored := freshDB(t, "evidence_policy_monitored")
	control := freshDB(t, "evidence_policy_control")
	hash := seedPolicy(t, control)
	action := seedExecutedDecision(t, monitored, &hash)
	m, rows, _ := buildPolicyPack(t, monitored, control, action)
	if len(rows) != 1 || rows[0].DocSHA256 != hash {
		t.Fatalf("policy.json = %+v, want the control database's v3", rows)
	}
	if m.PolicyHashMatch == nil || !*m.PolicyHashMatch {
		t.Fatalf("manifest policy_hash_match = %v, want true", m.PolicyHashMatch)
	}
	_, local, _ := buildPolicyPack(t, monitored, monitored, action)
	if len(local) != 0 {
		t.Fatalf("the monitored database served policy rows %+v, want none", local)
	}
}

// A decision written before policy-03 names a version and no hash: the
// pack still holds that version, and there is no hash to compare.
func TestLegacyDecisionWithoutHashHasNoHashMatch(t *testing.T) {
	pool := freshDB(t, "evidence_policy_legacy")
	seedPolicy(t, pool)
	action := seedExecutedDecision(t, pool, nil)
	m, rows, files := buildPolicyPack(t, pool, pool, action)
	if len(rows) != 1 || rows[0].Version != 3 {
		t.Fatalf("policy.json = %+v, want v3", rows)
	}
	if m.PolicyHashMatch != nil {
		t.Fatalf("policy_hash_match = %v, want absent", *m.PolicyHashMatch)
	}
	if bytes.Contains(files["manifest.json"], []byte("policy_hash_match")) {
		t.Fatalf("manifest names policy_hash_match with nothing to compare")
	}
}

// audit-07: a nil Control now means "the policy is in the audited
// database" (standalone), so the pack still needs the audited database.
func TestBuildNeedsTheLedgerDatabase(t *testing.T) {
	pool := freshDB(t, "evidence_policy_nil")
	action := seedExecutedDecision(t, pool, nil)
	if _, err := Build(context.Background(), Sources{Control: pool},
		Scope{ActionID: action}, nil, time.Now()); err == nil {
		t.Fatal("a pack was built without the audited database")
	}
}
