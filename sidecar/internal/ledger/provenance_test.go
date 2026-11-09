package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// policy-03: sage.decision declared guardrails and off_window_ok but no
// write ever set them, and the policy a decision was made under was a
// version number only. Every write now stores the guardrails, the deadline
// override and the SHA-256 of the evaluated policy document.

const provenanceHash = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

func provenanceDecision(evidenceID string) DecisionInput {
	input := validPostgresDecision(evidenceID)
	input.Feature, input.Intent = "freeze", "freeze"
	input.Reason = "deadline_override"
	input.Guardrails = []string{"concurrently", "lock_timeout"}
	input.OffWindowOK = true
	input.PolicyVersion, input.PolicySHA256 = 4, provenanceHash
	hardAt := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	input.DeadlineKind, input.DeadlineHardAt = "xid", &hardAt
	return input
}

type storedProvenance struct {
	guardrails  []string
	offWindowOK bool
	version     int
	hash        *string
}

func readProvenance(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64,
) storedProvenance {
	t.Helper()
	var got storedProvenance
	var raw []byte
	err := pool.QueryRow(ctx, `SELECT guardrails, off_window_ok, policy_version,
		policy_doc_sha256 FROM sage.decision WHERE id = $1`, id).Scan(&raw,
		&got.offWindowOK, &got.version, &got.hash)
	if err != nil {
		t.Fatalf("read decision %d provenance: %v", id, err)
	}
	if err := json.Unmarshal(raw, &got.guardrails); err != nil {
		t.Fatalf("decode guardrails %s: %v", raw, err)
	}
	return got
}

func TestDeadlineOverrideStoresOffWindowGuardrailsAndHash(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	input := provenanceDecision(ledgerEvidenceID("p03-execute"))
	decision, err := NewService(NewPostgresRepository(pool)).RecordDecision(ctx, input)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	t.Cleanup(func() { deleteLedgerDecision(pool, decision.ID) })
	got := readProvenance(t, ctx, pool, decision.ID)
	if !got.offWindowOK || got.version != 4 || got.hash == nil || *got.hash != provenanceHash ||
		!reflect.DeepEqual(got.guardrails, input.Guardrails) {
		t.Fatalf("stored provenance = %+v (hash %v), want off-window v4 %s %v", got,
			deref(got.hash), provenanceHash, input.Guardrails)
	}
}

// The fingerprint upsert writes them on the first row and keeps the latest
// guardrails and override on a repeat; the hash of the version is fixed.
func TestFingerprintedDecisionStoresAndRefreshesProvenance(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	repo := NewPostgresRepository(pool)
	databaseID := ledgerDatabaseID()
	input := provenanceDecision(ledgerEvidenceID("p03-park"))
	input.DatabaseID, input.Verdict = &databaseID, VerdictPark
	input.Fingerprint = DecisionFingerprint(input)
	id, _, err := repo.UpsertDecision(ctx, input)
	if err != nil {
		t.Fatalf("UpsertDecision: %v", err)
	}
	t.Cleanup(func() { deleteLedgerDecision(pool, id) })
	first := readProvenance(t, ctx, pool, id)
	if !first.offWindowOK || first.hash == nil || *first.hash != provenanceHash ||
		len(first.guardrails) != 2 {
		t.Fatalf("first row provenance = %+v, want the input's", first)
	}
	repeat := input
	repeat.EvidenceID = ledgerEvidenceID("p03-park-repeat")
	repeat.Guardrails, repeat.OffWindowOK = []string{"approval_required"}, false
	again, _, err := repo.UpsertDecision(ctx, repeat)
	if err != nil || again != id {
		t.Fatalf("repeat upsert = %d, %v; want the same row %d", again, err, id)
	}
	got := readProvenance(t, ctx, pool, id)
	if got.offWindowOK || !reflect.DeepEqual(got.guardrails, repeat.Guardrails) ||
		got.hash == nil || *got.hash != provenanceHash {
		t.Fatalf("repeat provenance = %+v, want the latest guardrails and the hash", got)
	}
}

// A hard stop is decided before any policy read: version 0, no hash.
func TestHardStopDecisionStoresVersionZeroAndNullHash(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	input := validPostgresDecision(ledgerEvidenceID("p03-hardstop"))
	input.Verdict, input.Reason = VerdictBlocked, "emergency_stop"
	input.PolicyVersion, input.PolicySHA256 = 0, ""
	decision, err := NewService(NewPostgresRepository(pool)).RecordDecision(ctx, input)
	if err != nil {
		t.Fatalf("RecordDecision of a hard stop: %v", err)
	}
	t.Cleanup(func() { deleteLedgerDecision(pool, decision.ID) })
	got := readProvenance(t, ctx, pool, decision.ID)
	if got.version != 0 || got.hash != nil || got.offWindowOK ||
		!reflect.DeepEqual(got.guardrails, []string{}) {
		t.Fatalf("hard stop provenance = %+v (hash %v), want v0, NULL, no guardrails",
			got, deref(got.hash))
	}
}

func TestDecisionHashCheckRejectsMalformedHashes(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	for _, hash := range []string{"", "abc", strings.ToUpper(provenanceHash),
		provenanceHash + "0", strings.Repeat("g", 64)} {
		_, err := pool.Exec(ctx, `INSERT INTO sage.decision (feature, intent, verdict,
			risk_tier, reason, evidence_id, policy_version, policy_doc_sha256)
			VALUES ('index', 'index', 'blocked', 'safe', 'x', $1, 1, $2)`,
			ledgerEvidenceID("p03-bad"), hash)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("hash %q: err = %v, want a CHECK violation (23514)", hash, err)
		}
	}
}

func TestRecordDecisionValidatesPolicyProvenance(t *testing.T) {
	tests := []struct {
		name string
		edit func(*DecisionInput)
		want string
	}{
		{"negative version", func(in *DecisionInput) { in.PolicyVersion = -1 },
			"policy_version"},
		{"short hash", func(in *DecisionInput) { in.PolicySHA256 = "abc" },
			"policy_doc_sha256"},
		{"upper-case hash", func(in *DecisionInput) {
			in.PolicySHA256 = strings.ToUpper(provenanceHash)
		}, "policy_doc_sha256"},
		{"hash without a version", func(in *DecisionInput) {
			in.PolicyVersion, in.PolicySHA256 = 0, provenanceHash
		}, "policy_version"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := &fakeRepository{}
			input := validDecisionInput()
			test.edit(&input)
			_, err := NewService(repo).RecordDecision(context.Background(), input)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want one naming %q", err, test.want)
			}
			if repo.insertCalls != 0 {
				t.Fatalf("insert calls = %d, want 0", repo.insertCalls)
			}
		})
	}
}

func TestRecordDecisionAcceptsVersionZeroWithoutHash(t *testing.T) {
	repo := &fakeRepository{}
	input := validDecisionInput()
	input.PolicyVersion, input.PolicySHA256 = 0, ""
	if _, err := NewService(repo).RecordDecision(context.Background(), input); err != nil {
		t.Fatalf("a decision made before any policy read was refused: %v", err)
	}
	if repo.insertCalls != 1 || repo.inserted.PolicyVersion != 0 {
		t.Fatalf("insert calls = %d, version %d; want one insert at v0", repo.insertCalls,
			repo.inserted.PolicyVersion)
	}
}

// decisionValues passes the provenance through: guardrails as a JSON
// array (never null), the override, and the hash or NULL.
func TestDecisionValuesEncodeProvenance(t *testing.T) {
	values, err := decisionValues(provenanceDecision("ev-values"))
	if err != nil {
		t.Fatalf("decisionValues: %v", err)
	}
	if len(values) != decisionParamCount {
		t.Fatalf("%d values, want %d", len(values), decisionParamCount)
	}
	if string(values[guardrailsParam].([]byte)) != `["concurrently","lock_timeout"]` ||
		values[offWindowParam] != true || values[policyHashParam] != provenanceHash {
		t.Fatalf("provenance values = %s %v %v", values[guardrailsParam],
			values[offWindowParam], values[policyHashParam])
	}
	bare, err := decisionValues(validPostgresDecision("ev-bare"))
	if err != nil {
		t.Fatalf("decisionValues: %v", err)
	}
	if string(bare[guardrailsParam].([]byte)) != `[]` || bare[offWindowParam] != false ||
		bare[policyHashParam] != nil {
		t.Fatalf("bare provenance values = %s %v %v, want [] false nil",
			bare[guardrailsParam], bare[offWindowParam], bare[policyHashParam])
	}
}

func deref(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return *value
}
