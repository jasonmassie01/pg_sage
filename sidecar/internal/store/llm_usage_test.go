package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fleet-10: rows are validated before any SQL, so a bad row never reaches
// the control database. A nil pool proves no query is attempted.

func TestLLMUsageFlush_RejectsInvalidRows(t *testing.T) {
	s := NewLLMUsageStore(nil)
	day := time.Date(2031, 3, 14, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		row  LLMUsageRow
		want string
	}{
		{"negative", LLMUsageRow{DatabaseKey: "name:a", Role: "fleet", Tokens: -1},
			"negative"},
		{"empty key", LLMUsageRow{DatabaseKey: "", Role: "fleet", Tokens: 1},
			"database key"},
		{"unknown role", LLMUsageRow{DatabaseKey: "name:a", Role: "admin", Tokens: 1},
			"admin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.Flush(context.Background(), day, []LLMUsageRow{tc.row})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("flush %+v = %v, want an error naming %q", tc.row, err, tc.want)
			}
		})
	}
}

// Nothing to flush is not a query: a nil pool would panic if it were.
func TestLLMUsageFlush_EmptyIsANoOp(t *testing.T) {
	s := NewLLMUsageStore(nil)
	if err := s.Flush(context.Background(), time.Now(), nil); err != nil {
		t.Fatalf("empty flush = %v, want nil", err)
	}
}

func TestLLMUsageStore_NilPoolFailsWithContext(t *testing.T) {
	s := NewLLMUsageStore(nil)
	_, err := s.Load(context.Background(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "control database") {
		t.Fatalf("load with no pool = %v, want an error naming the control database", err)
	}
	err = s.Flush(context.Background(), time.Now(),
		[]LLMUsageRow{{DatabaseKey: "name:a", Role: "fleet", Tokens: 1}})
	if err == nil || !strings.Contains(err.Error(), "control database") {
		t.Fatalf("flush with no pool = %v, want an error naming the control database", err)
	}
}

func TestMergeLLMUsageRows_KeepsMaxPerKey(t *testing.T) {
	got := mergeLLMUsageRows([]LLMUsageRow{
		{DatabaseKey: "name:a", Role: "fleet", Tokens: 5},
		{DatabaseKey: "name:a", Role: "fleet", Tokens: 9},
		{DatabaseKey: "name:a", Role: "general", Tokens: 2},
		{DatabaseKey: "name:a", Role: "fleet", Tokens: 7},
	})
	if len(got) != 2 {
		t.Fatalf("merged = %+v, want 2 rows", got)
	}
	for _, row := range got {
		if row.Role == "fleet" && row.Tokens != 9 {
			t.Fatalf("fleet row = %+v, want 9", row)
		}
		if row.Role == "general" && row.Tokens != 2 {
			t.Fatalf("general row = %+v, want 2", row)
		}
	}
}
