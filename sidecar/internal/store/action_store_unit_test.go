package store

import (
	"testing"
	"time"
)

func TestNilIfEmpty_EmptyString(t *testing.T) {
	got := NilIfEmpty("")
	if got != nil {
		t.Errorf("NilIfEmpty(\"\") = %v, want nil", got)
	}
}

func TestNilIfEmpty_NonEmptyString(t *testing.T) {
	got := NilIfEmpty("ROLLBACK SQL")
	if got == nil {
		t.Fatal("NilIfEmpty(\"ROLLBACK SQL\") = nil, want non-nil")
	}
	if *got != "ROLLBACK SQL" {
		t.Errorf("NilIfEmpty(\"ROLLBACK SQL\") = %q, want %q",
			*got, "ROLLBACK SQL")
	}
}

func TestNilIfEmpty_WhitespaceOnly(t *testing.T) {
	// Whitespace-only is NOT empty per the function's logic.
	got := NilIfEmpty("   ")
	if got == nil {
		t.Fatal("NilIfEmpty(\"   \") = nil, want non-nil pointer")
	}
	if *got != "   " {
		t.Errorf("NilIfEmpty(\"   \") = %q, want %q", *got, "   ")
	}
}

func TestNilIfEmpty_SingleChar(t *testing.T) {
	got := NilIfEmpty("x")
	if got == nil {
		t.Fatal("NilIfEmpty(\"x\") = nil, want non-nil")
	}
	if *got != "x" {
		t.Errorf("got %q, want %q", *got, "x")
	}
}

func TestNilIfEmpty_TableDriven(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantNil bool
		wantVal string
	}{
		{"empty", "", true, ""},
		{"simple", "DROP INDEX idx", false, "DROP INDEX idx"},
		{"newlines", "SELECT\n1", false, "SELECT\n1"},
		{"tab", "\t", false, "\t"},
		{
			"long SQL",
			"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx ON t(c)",
			false,
			"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx ON t(c)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NilIfEmpty(tt.input)
			if tt.wantNil {
				if got != nil {
					t.Errorf("NilIfEmpty(%q) = %q, want nil",
						tt.input, *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("NilIfEmpty(%q) = nil, want %q",
					tt.input, tt.wantVal)
			}
			if *got != tt.wantVal {
				t.Errorf("NilIfEmpty(%q) = %q, want %q",
					tt.input, *got, tt.wantVal)
			}
		})
	}
}

func TestQueuedAction_ZeroValue(t *testing.T) {
	var a QueuedAction

	if a.ID != 0 {
		t.Errorf("zero QueuedAction.ID = %d, want 0", a.ID)
	}
	if a.DatabaseID != nil {
		t.Errorf("zero QueuedAction.DatabaseID = %v, want nil",
			a.DatabaseID)
	}
	if a.FindingID != 0 {
		t.Errorf("zero QueuedAction.FindingID = %d, want 0", a.FindingID)
	}
	if a.ProposedSQL != "" {
		t.Errorf("zero QueuedAction.ProposedSQL = %q, want empty",
			a.ProposedSQL)
	}
	if a.RollbackSQL != "" {
		t.Errorf("zero QueuedAction.RollbackSQL = %q, want empty",
			a.RollbackSQL)
	}
	if a.ActionRisk != "" {
		t.Errorf("zero QueuedAction.ActionRisk = %q, want empty",
			a.ActionRisk)
	}
	if a.Status != "" {
		t.Errorf("zero QueuedAction.Status = %q, want empty",
			a.Status)
	}
	if !a.ProposedAt.IsZero() {
		t.Errorf("zero QueuedAction.ProposedAt = %v, want zero time",
			a.ProposedAt)
	}
	if a.DecidedBy != nil {
		t.Errorf("zero QueuedAction.DecidedBy = %v, want nil",
			a.DecidedBy)
	}
	if a.DecidedAt != nil {
		t.Errorf("zero QueuedAction.DecidedAt = %v, want nil",
			a.DecidedAt)
	}
	if !a.ExpiresAt.IsZero() {
		t.Errorf("zero QueuedAction.ExpiresAt = %v, want zero time",
			a.ExpiresAt)
	}
	if a.Reason != "" {
		t.Errorf("zero QueuedAction.Reason = %q, want empty", a.Reason)
	}
}

func TestQueuedAction_PopulatedFields(t *testing.T) {
	dbID := 5
	userID := 10
	now := time.Now()

	a := QueuedAction{
		ID:          1,
		DatabaseID:  &dbID,
		FindingID:   42,
		ProposedSQL: "CREATE INDEX CONCURRENTLY idx ON t(c)",
		RollbackSQL: "DROP INDEX CONCURRENTLY idx",
		ActionRisk:  "safe",
		Status:      "pending",
		ProposedAt:  now,
		DecidedBy:   &userID,
		DecidedAt:   &now,
		ExpiresAt:   now.Add(24 * time.Hour),
		Reason:      "auto-approved",
	}

	if a.ID != 1 {
		t.Errorf("ID = %d, want 1", a.ID)
	}
	if a.DatabaseID == nil || *a.DatabaseID != 5 {
		t.Errorf("DatabaseID = %v, want 5", a.DatabaseID)
	}
	if a.FindingID != 42 {
		t.Errorf("FindingID = %d, want 42", a.FindingID)
	}
	if a.ProposedSQL != "CREATE INDEX CONCURRENTLY idx ON t(c)" {
		t.Errorf("ProposedSQL = %q", a.ProposedSQL)
	}
	if a.RollbackSQL != "DROP INDEX CONCURRENTLY idx" {
		t.Errorf("RollbackSQL = %q", a.RollbackSQL)
	}
	if a.ActionRisk != "safe" {
		t.Errorf("ActionRisk = %q, want safe", a.ActionRisk)
	}
	if a.Status != "pending" {
		t.Errorf("Status = %q, want pending", a.Status)
	}
	if a.DecidedBy == nil || *a.DecidedBy != 10 {
		t.Errorf("DecidedBy = %v, want 10", a.DecidedBy)
	}
	if a.Reason != "auto-approved" {
		t.Errorf("Reason = %q, want auto-approved", a.Reason)
	}
	if a.ExpiresAt.Before(now) {
		t.Errorf("ExpiresAt %v is before ProposedAt %v",
			a.ExpiresAt, now)
	}
}

func TestConfigOverride_ZeroValue(t *testing.T) {
	var o ConfigOverride

	if o.Key != "" {
		t.Errorf("zero ConfigOverride.Key = %q, want empty", o.Key)
	}
	if o.Value != "" {
		t.Errorf("zero ConfigOverride.Value = %q, want empty", o.Value)
	}
	if o.DatabaseID != 0 {
		t.Errorf("zero ConfigOverride.DatabaseID = %d, want 0",
			o.DatabaseID)
	}
	if !o.UpdatedAt.IsZero() {
		t.Errorf("zero ConfigOverride.UpdatedAt is not zero time")
	}
	if o.UpdatedBy != 0 {
		t.Errorf("zero ConfigOverride.UpdatedBy = %d, want 0",
			o.UpdatedBy)
	}
}

func TestConfigAuditEntry_ZeroValue(t *testing.T) {
	var e ConfigAuditEntry

	if e.ID != 0 {
		t.Errorf("zero ConfigAuditEntry.ID = %d, want 0", e.ID)
	}
	if e.Key != "" {
		t.Errorf("zero ConfigAuditEntry.Key = %q, want empty", e.Key)
	}
	if e.OldValue != "" {
		t.Errorf("zero ConfigAuditEntry.OldValue = %q, want empty",
			e.OldValue)
	}
	if e.NewValue != "" {
		t.Errorf("zero ConfigAuditEntry.NewValue = %q, want empty",
			e.NewValue)
	}
	if e.DatabaseID != 0 {
		t.Errorf("zero ConfigAuditEntry.DatabaseID = %d, want 0",
			e.DatabaseID)
	}
	if e.ChangedBy != 0 {
		t.Errorf("zero ConfigAuditEntry.ChangedBy = %d, want 0",
			e.ChangedBy)
	}
	if !e.ChangedAt.IsZero() {
		t.Errorf("zero ConfigAuditEntry.ChangedAt is not zero time")
	}
}
