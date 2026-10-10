package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// legacyFingerprint is DecisionFingerprint as it was before agent
// provenance, copied verbatim.
func legacyFingerprint(input DecisionInput) string {
	database := ""
	if input.DatabaseID != nil {
		database = strconv.Itoa(*input.DatabaseID)
	}
	targets := append([]string(nil), input.TargetObjects...)
	sort.Strings(targets)
	sum := sha256.Sum256([]byte(strings.Join([]string{
		database, input.Feature, input.Intent, strings.Join(targets, "\x1e"),
		string(input.Verdict), strconv.Itoa(input.PolicyVersion), input.ProposedSQL,
	}, "\x1f")))
	return "fp_" + hex.EncodeToString(sum[:16])
}

// Agent provenance on sage.decision (AGENTDB-SPEC §6.2.3, §7):
// principal_id, task_id and artifact_hash.

func TestFingerprintUnchangedWithoutPrincipal(t *testing.T) {
	in := validPostgresDecision("fp")
	in.Verdict = VerdictQueueApproval
	// The pre-G1 fingerprint, pinned: existing open rows keep matching.
	if got := DecisionFingerprint(in); got != legacyFingerprint(in) {
		t.Fatalf("fingerprint = %s, want the legacy value %s", got, legacyFingerprint(in))
	}
	in.TaskID, in.ArtifactHash = "t-1", "sha256:x" // only the principal separates
	if DecisionFingerprint(in) != legacyFingerprint(in) {
		t.Fatal("task and artifact hash alone must not change a non-agent fingerprint")
	}
}

func TestFingerprintSeparatesPrincipals(t *testing.T) {
	a := validPostgresDecision("fp")
	a.Verdict = VerdictQueueApproval
	b, own := a, a
	a.PrincipalID = "agp_aaaaaaaaaaaaaaaaaaaa"
	b.PrincipalID = "agp_bbbbbbbbbbbbbbbbbbbb"
	if DecisionFingerprint(a) == DecisionFingerprint(b) ||
		DecisionFingerprint(a) == DecisionFingerprint(own) {
		t.Fatal("decisions of different agents (or pg_sage's own) must not share a row")
	}
}

func TestDecisionPrincipalPersists(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	repo := NewPostgresRepository(pool)
	in := validPostgresDecision(ledgerEvidenceID("agent"))
	in.PrincipalID, in.TaskID = "agp_aaaaaaaaaaaaaaaaaaaa", "task-7"
	in.ArtifactHash = "sha256:0123"
	id, err := repo.InsertDecision(ctx, in)
	if err != nil {
		t.Fatalf("InsertDecision: %v", err)
	}
	t.Cleanup(func() { deleteLedgerDecision(pool, id) })
	var principal, task, hash *string
	if err := pool.QueryRow(ctx, `SELECT principal_id, task_id, artifact_hash
		FROM sage.decision WHERE id = $1`, id).Scan(&principal, &task, &hash); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if principal == nil || *principal != in.PrincipalID || task == nil ||
		*task != "task-7" || hash == nil || *hash != "sha256:0123" {
		t.Fatalf("persisted = %v %v %v", principal, task, hash)
	}
	// pg_sage's own decision stores NULLs, not empty strings.
	own := validPostgresDecision(ledgerEvidenceID("own"))
	id2, err := repo.InsertDecision(ctx, own)
	if err != nil {
		t.Fatalf("InsertDecision: %v", err)
	}
	t.Cleanup(func() { deleteLedgerDecision(pool, id2) })
	if err := pool.QueryRow(ctx, `SELECT principal_id, task_id, artifact_hash
		FROM sage.decision WHERE id = $1`, id2).Scan(&principal, &task, &hash); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if principal != nil || task != nil || hash != nil {
		t.Fatalf("own decision = %v %v %v, want NULLs", principal, task, hash)
	}
}

func TestUpsertDecisionPrincipalPersists(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	repo := NewPostgresRepository(pool)
	in := validPostgresDecision(ledgerEvidenceID("agent-up"))
	in.Verdict = VerdictQueueApproval
	db := ledgerDatabaseID()
	in.DatabaseID = &db
	in.PrincipalID = "agp_cccccccccccccccccccc"
	in.Fingerprint = DecisionFingerprint(in)
	id, _, err := repo.UpsertDecision(ctx, in)
	if err != nil {
		t.Fatalf("UpsertDecision: %v", err)
	}
	t.Cleanup(func() { deleteLedgerDecision(pool, id) })
	var principal *string
	if err := pool.QueryRow(ctx, `SELECT principal_id FROM sage.decision WHERE id = $1`,
		id).Scan(&principal); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if principal == nil || *principal != in.PrincipalID {
		t.Fatalf("principal = %v", principal)
	}
}
