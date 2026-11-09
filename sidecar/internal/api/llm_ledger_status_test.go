package api

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
)

// fleet-10: /api/v1/llm/status says when today's usage was restored from
// the control database and when it was last flushed there.

type ledgerRegistry struct {
	*llm.Manager
	restored, flushed time.Time
}

func (r ledgerRegistry) UsageLedgerStatus() (time.Time, time.Time) {
	return r.restored, r.flushed
}

func TestLLMStatusHandler_ReportsLedgerTimes(t *testing.T) {
	restored := time.Date(2026, 11, 8, 9, 30, 0, 0, time.UTC)
	flushed := restored.Add(90 * time.Second)
	body := getLLMStatus(t, ledgerRegistry{
		Manager:  llm.NewManager(testLLMClient("m", 1000), nil, false),
		restored: restored, flushed: flushed,
	})
	if got := string(body["usage_restored_at"]); got != `"2026-11-08T09:30:00Z"` {
		t.Fatalf("usage_restored_at = %s", got)
	}
	if got := string(body["ledger_last_flush_at"]); got != `"2026-11-08T09:31:30Z"` {
		t.Fatalf("ledger_last_flush_at = %s", got)
	}
}

// Before the first restore or flush both fields are null, not a zero time.
func TestLLMStatusHandler_LedgerTimesNullUntilSet(t *testing.T) {
	body := getLLMStatus(t, ledgerRegistry{
		Manager: llm.NewManager(testLLMClient("m", 1000), nil, false),
	})
	for _, field := range []string{"usage_restored_at", "ledger_last_flush_at"} {
		raw, ok := body[field]
		if !ok || string(raw) != "null" {
			t.Fatalf("%s = %s (present=%v), want null", field, raw, ok)
		}
	}
}

// A registry without a ledger omits both fields.
func TestLLMStatusHandler_NoLedgerFieldsWithoutReporter(t *testing.T) {
	body := getLLMStatus(t, llm.NewManager(testLLMClient("m", 1000), nil, false))
	for _, field := range []string{"usage_restored_at", "ledger_last_flush_at"} {
		if raw, ok := body[field]; ok {
			t.Fatalf("%s = %s, want absent for a plain manager", field, raw)
		}
	}
}
