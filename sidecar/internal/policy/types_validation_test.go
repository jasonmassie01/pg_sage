package policy

import (
	"strings"
	"testing"
	"time"
)

func TestActionRequestAndDecisionPreserveTypedSafetyFields(t *testing.T) {
	hardAt := time.Date(2026, 7, 22, 5, 0, 0, 0, time.UTC)
	contract := &ActionContract{
		ActionType: "create_index_concurrently",
		RiskTier:   RiskSafe,
		Guardrails: []Guardrail{GuardrailApprovalRequired},
	}
	req := ActionRequest{
		Contract:   contract,
		SQL:        `CREATE INDEX CONCURRENTLY idx_orders ON public.orders (status)`,
		TargetObjs: []string{"public.orders"},
		Feature:    "index",
		Deadline: &DeadlineContext{
			Kind:    DeadlineDisk,
			Urgency: UrgencyCritical,
			HardAt:  hardAt,
		},
	}
	decision := Decision{
		Verdict:     VerdictQueueApproval,
		RiskTier:    RiskSafe,
		Reason:      ReasonApprovalRequired,
		Guardrails:  []Guardrail{GuardrailApprovalRequired},
		OffWindowOK: false,
		EvidenceID:  "decision-42",
	}

	if req.Contract.ActionType != "create_index_concurrently" {
		t.Fatalf("ActionType = %q", req.Contract.ActionType)
	}
	if req.Deadline.Kind != DeadlineDisk || !req.Deadline.HardAt.Equal(hardAt) {
		t.Fatalf("Deadline = %#v", req.Deadline)
	}
	if decision.Verdict != VerdictQueueApproval ||
		decision.Reason != ReasonApprovalRequired {
		t.Fatalf("Decision = %#v", decision)
	}
	if len(decision.Guardrails) != 1 ||
		decision.Guardrails[0] != GuardrailApprovalRequired {
		t.Fatalf("Guardrails = %#v", decision.Guardrails)
	}
	if decision.EvidenceID != "decision-42" {
		t.Fatalf("EvidenceID = %q", decision.EvidenceID)
	}
}

func TestParseDocumentRejectsUnknownFields(t *testing.T) {
	raw := validPolicyJSON(`"storage_bytes": null`)
	raw = strings.Replace(raw, `"serialize_mode":"park"`,
		`"serialize_mode":"park","surprise":true`, 1)

	_, err := ParseDocument([]byte(raw))

	if err == nil || !strings.Contains(err.Error(), "surprise") {
		t.Fatalf("ParseDocument error = %v, want unknown-field detail", err)
	}
}

func TestParseDocumentDistinguishesNullZeroAndMissingBudget(t *testing.T) {
	tests := []struct {
		name      string
		budget    string
		wantErr   string
		wantValue *int64
		wantNoCap bool
	}{
		{
			name:      "explicit null means no cap",
			budget:    `"storage_bytes": null`,
			wantNoCap: true,
		},
		{
			name:      "explicit zero means none allowed",
			budget:    `"storage_bytes": 0`,
			wantValue: int64Pointer(0),
		},
		{
			name:    "missing dimension fails closed",
			budget:  ``,
			wantErr: "storage_bytes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := ParseDocument([]byte(validPolicyJSON(tt.budget)))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDocument: %v", err)
			}
			limit := doc.Budgets.StorageBytes
			if !limit.Present {
				t.Fatal("StorageBytes.Present = false, want true")
			}
			if limit.NoCap() != tt.wantNoCap {
				t.Fatalf("NoCap() = %v, want %v", limit.NoCap(), tt.wantNoCap)
			}
			if tt.wantValue != nil {
				value, ok := limit.Value()
				if !ok || value != *tt.wantValue {
					t.Fatalf("Value() = (%d, %v), want (%d, true)",
						value, ok, *tt.wantValue)
				}
			}
		})
	}
}

func TestValidateDocumentRejectsAmbiguityInsteadOfCoercing(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Document)
		want string
	}{
		{
			name: "unknown classification cannot be permissive",
			edit: func(doc *Document) { doc.UnknownClassification = "allow" },
			want: "unknown_classification",
		},
		{
			name: "unknown serialize mode",
			edit: func(doc *Document) { doc.SerializeMode = "wait_forever" },
			want: "serialize_mode",
		},
		{
			name: "negative lock ceiling",
			edit: func(doc *Document) { doc.LockDurationCeilingMS = -1 },
			want: "lock_duration_ceiling_ms",
		},
		{
			name: "negative blast radius",
			edit: func(doc *Document) { doc.BlastRadius.MaxRowsRewritten = -1 },
			want: "max_rows_rewritten",
		},
		{
			name: "unknown change class",
			edit: func(doc *Document) {
				doc.AllowedChangeClasses = append(doc.AllowedChangeClasses, "magic")
			},
			want: "magic",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := StaffedProfile()
			tt.edit(&doc)

			err := ValidateDocument(doc)

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateDocument error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateDocumentRejectsUnknownTypedGuardrail(t *testing.T) {
	contract := ActionContract{
		ActionType: "analyze_table",
		RiskTier:   RiskSafe,
		Guardrails: []Guardrail{Guardrail("hope_for_the_best")},
	}

	err := ValidateContract(contract)

	if err == nil || !strings.Contains(err.Error(), "hope_for_the_best") {
		t.Fatalf("ValidateContract error = %v, want unknown guardrail", err)
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}

func validPolicyJSON(storageBudget string) string {
	storageField := storageBudget
	if storageField != "" {
		storageField += ","
	}
	return `{
  "allowed_change_classes":["index","analyze","vacuum","freeze"],
  "maintenance_windows":["weekdays 01:00-05:00"],
  "lock_duration_ceiling_ms":3000,
  "blast_radius":{"max_rows_rewritten":5000000,"max_tables_per_window":20},
  "budgets":{` + storageField + `"spend_daily":null,"llm_tokens_daily":500000},
  "rate_limits":{"max_self_initiated_changes_per_window":50},
  "deadline_overrides":{"xid":false,"disk":false},
  "refusal_set":["rls_change","grant_expansion","major_upgrade"],
  "unknown_classification":"fail_closed",
  "serialize_mode":"park"
}`
}
