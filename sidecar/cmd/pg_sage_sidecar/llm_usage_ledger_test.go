package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/store"
)

// fleet-10: the usage ledger persists today's LLM spend to the control
// database and restores it at startup, so a restart mid-day does not hand
// out the full daily allocation again.

type memUsageStore struct {
	mu       sync.Mutex
	rows     map[string]map[string]int64 // day -> "key|role" -> tokens
	failing  bool
	loaded   []time.Time
	cleared  []time.Time
	flushes  int
	loadFail bool
}

func newMemUsageStore() *memUsageStore {
	return &memUsageStore{rows: map[string]map[string]int64{}}
}

var errLedgerDown = errors.New("dial tcp 127.0.0.1:5432: connection refused")

func (m *memUsageStore) Flush(_ context.Context, day time.Time, rows []store.LLMUsageRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failing {
		return errLedgerDown
	}
	m.flushes++
	byKey := m.rows[day.Format(time.DateOnly)]
	if byKey == nil {
		byKey = map[string]int64{}
		m.rows[day.Format(time.DateOnly)] = byKey
	}
	for _, row := range rows {
		k := row.DatabaseKey + "|" + row.Role
		byKey[k] = max(byKey[k], row.Tokens)
	}
	return nil
}

func (m *memUsageStore) Load(_ context.Context, day time.Time) ([]store.LLMUsageRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loaded = append(m.loaded, day)
	if m.failing || m.loadFail {
		return nil, errLedgerDown
	}
	var out []store.LLMUsageRow
	for k, tokens := range m.rows[day.Format(time.DateOnly)] {
		var key, role string
		for i := len(k) - 1; i >= 0; i-- {
			if k[i] == '|' {
				key, role = k[:i], k[i+1:]
				break
			}
		}
		out = append(out, store.LLMUsageRow{DatabaseKey: key, Role: role, Tokens: tokens})
	}
	return out, nil
}

func (m *memUsageStore) Clear(_ context.Context, day time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failing {
		return errLedgerDown
	}
	m.cleared = append(m.cleared, day)
	delete(m.rows, day.Format(time.DateOnly))
	return nil
}

func (m *memUsageStore) Prune(context.Context, time.Time) (int64, error) { return 0, nil }

func (m *memUsageStore) get(day time.Time, key, role string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rows[day.Format(time.DateOnly)][key+"|"+role]
}

func (m *memUsageStore) set(day time.Time, key, role string, tokens int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows[day.Format(time.DateOnly)] == nil {
		m.rows[day.Format(time.DateOnly)] = map[string]int64{}
	}
	m.rows[day.Format(time.DateOnly)][key+"|"+role] = tokens
}

func (m *memUsageStore) setFailing(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failing = v
}

func ledgerToday() time.Time { return time.Now().UTC().Truncate(24 * time.Hour) }

// fleetLedgerRuntime builds the fleet LLM runtime fresh, as a restart does.
func fleetLedgerRuntime(t *testing.T, tokensPerCall int) {
	t.Helper()
	llmTestGlobals(t)
	cfg.LLM.FleetTokenBudgetDaily = 1000
	initializeFleetBudget(nil)
	pointLLMAt(t, fakeLLMProvider(t, tokensPerCall).URL)
}

type warnCounter struct {
	mu    sync.Mutex
	lines []string
}

func (w *warnCounter) warn(format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, fmt.Sprintf(format, args...))
}

func (w *warnCounter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.lines)
}

func testLedger(st llmUsageStore, keys *llmUsageKeys) (*llmUsageLedger, *warnCounter) {
	l := newLLMUsageLedger(st, llmClients, fleetLLMBudget, keys)
	w := &warnCounter{}
	l.warn = w.warn
	return l, w
}

func TestLLMUsageLedger_RestartRestoresTodaysUsage(t *testing.T) {
	fleetLedgerRuntime(t, 400)
	general, _ := newFleetDBLLMClients("orders", true)
	ctx := context.Background()
	if _, _, err := general.Chat(ctx, "sys", "spend 400", 10); err != nil {
		t.Fatalf("chat: %v", err)
	}
	mem := newMemUsageStore()
	keys := newLLMUsageKeys()
	before, _ := testLedger(mem, keys)
	if err := before.flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	today := ledgerToday()
	for _, want := range []struct {
		key, role string
		tokens    int64
	}{{"name:orders", "fleet", 400}, {"name:orders", "general", 400},
		{"process", "shared", 400}} {
		if got := mem.get(today, want.key, want.role); got != want.tokens {
			t.Fatalf("ledger %s/%s = %d, want %d", want.key, want.role, got, want.tokens)
		}
	}

	// Restart: every budget and client is rebuilt from zero.
	fleetLedgerRuntime(t, 400)
	rebuilt, _ := newFleetDBLLMClients("orders", true)
	if fleetLLMBudget.Used("orders") != 0 || rebuilt.TokensUsedToday() != 0 {
		t.Fatal("precondition: a rebuilt runtime starts at zero")
	}
	after, _ := testLedger(mem, keys)
	if err := after.restore(ctx); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := fleetLLMBudget.Used("orders"); got != 400 {
		t.Fatalf("fleet used after restore = %d, want 400", got)
	}
	if got := rebuilt.TokensUsedToday(); got != 400 {
		t.Fatalf("client counter after restore = %d, want 400", got)
	}
	if got := llmClients.GlobalBudget().Used; got != 400 {
		t.Fatalf("global used after restore = %d, want 400", got)
	}
	if len(mem.loaded) != 1 || !mem.loaded[0].Equal(today) {
		t.Fatalf("loaded days = %v, want only today %v", mem.loaded, today)
	}
	if got := llmClients.TokenStatus()["orders/fleet_budget"].TokensUsed; got != 400 {
		t.Fatalf("/llm/status fleet used = %d, want 400 (same as before restart)", got)
	}
}

// A database that registers after the restore gets its share on Register,
// and a client built after the restore starts from the restored count.
func TestLLMUsageLedger_RestoreHeldUntilRegister(t *testing.T) {
	fleetLedgerRuntime(t, 10)
	mem := newMemUsageStore()
	today := ledgerToday()
	mem.set(today, "name:billing", "fleet", 300)
	mem.set(today, "name:billing", "general", 200)
	mem.set(today, "name:billing", "optimizer", 50)
	l, _ := testLedger(mem, newLLMUsageKeys())
	if err := l.restore(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := fleetLLMBudget.Used("billing"); got != 0 {
		t.Fatalf("unregistered billing used = %d, want 0 before register", got)
	}
	general, manager := newFleetDBLLMClients("billing", true)
	if got := fleetLLMBudget.Used("billing"); got != 300 {
		t.Fatalf("billing fleet used after register = %d, want 300", got)
	}
	if got := general.TokensUsedToday(); got != 200 {
		t.Fatalf("billing general = %d, want 200", got)
	}
	if got := manager.Optimizer.TokensUsedToday(); got != 50 {
		t.Fatalf("billing optimizer = %d, want 50", got)
	}
}

// Meta-DB databases are keyed by id, so the key survives a rename.
func TestLLMUsageLedger_MetaDatabasesKeyedByID(t *testing.T) {
	fleetLedgerRuntime(t, 70)
	keys := newLLMUsageKeys()
	keys.setID("orders", 7)
	general, _ := newFleetDBLLMClients("orders", true)
	if _, _, err := general.Chat(context.Background(), "sys", "p", 10); err != nil {
		t.Fatal(err)
	}
	mem := newMemUsageStore()
	l, _ := testLedger(mem, keys)
	if err := l.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := mem.get(ledgerToday(), "id:7", "fleet"); got != 70 {
		t.Fatalf("id:7 fleet row = %d, want 70", got)
	}
	if got := mem.get(ledgerToday(), "name:orders", "fleet"); got != 0 {
		t.Fatalf("a meta database was also written by name: %d", got)
	}
	// After a restart under a new name the id still finds the row.
	fleetLedgerRuntime(t, 70)
	renamed := newLLMUsageKeys()
	renamed.setID("orders-renamed", 7)
	fleetLLMBudget.Register("orders-renamed")
	l2, _ := testLedger(mem, renamed)
	if err := l2.restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fleetLLMBudget.Used("orders-renamed"); got != 70 {
		t.Fatalf("renamed database used = %d, want 70 restored by id", got)
	}
}

func TestLLMUsageLedger_NoRowsMeansZero(t *testing.T) {
	fleetLedgerRuntime(t, 10)
	general, _ := newFleetDBLLMClients("orders", true)
	l, w := testLedger(newMemUsageStore(), newLLMUsageKeys())
	if restored, _ := l.status(); !restored.IsZero() {
		t.Fatalf("restored_at before restore = %v, want zero", restored)
	}
	if err := l.restore(context.Background()); err != nil {
		t.Fatalf("restore of an empty ledger: %v", err)
	}
	if fleetLLMBudget.Used("orders") != 0 || general.TokensUsedToday() != 0 {
		t.Fatal("an empty ledger restored usage")
	}
	if restored, _ := l.status(); restored.IsZero() {
		t.Fatal("restored_at must be set after a successful restore")
	}
	if w.count() != 0 {
		t.Fatalf("warnings = %v, want none", w.lines)
	}
}

// Rows with unknown keys or roles are skipped, not fatal.
func TestLLMUsageLedger_IgnoresUnknownRows(t *testing.T) {
	fleetLedgerRuntime(t, 10)
	mem := newMemUsageStore()
	today := ledgerToday()
	mem.set(today, "id:99", "fleet", 500) // no database has id 99
	mem.set(today, "bogus", "fleet", 500)
	l, _ := testLedger(mem, newLLMUsageKeys())
	if err := l.restore(context.Background()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	newFleetDBLLMClients("orders", true)
	if got := fleetLLMBudget.Used("orders"); got != 0 {
		t.Fatalf("orders used = %d, want 0", got)
	}
}

func TestLLMUsageLedger_UnreachableStoreFailsOpenWithOneWarning(t *testing.T) {
	fleetLedgerRuntime(t, 20)
	general, _ := newFleetDBLLMClients("orders", true)
	mem := newMemUsageStore()
	mem.setFailing(true)
	l, w := testLedger(mem, newLLMUsageKeys())
	ctx := context.Background()
	if err := l.restore(ctx); !errors.Is(err, errLedgerDown) {
		t.Fatalf("restore = %v, want the store error", err)
	}
	for i := 0; i < 3; i++ {
		if _, _, err := general.Chat(ctx, "sys", fmt.Sprintf("p%d", i), 10); err != nil {
			t.Fatalf("chat %d with the ledger down: %v", i, err)
		}
		if err := l.flush(ctx); !errors.Is(err, errLedgerDown) {
			t.Fatalf("flush %d = %v, want the store error", i, err)
		}
	}
	if !fleetLLMBudget.CanSpend("orders", 1) {
		t.Fatal("CanSpend must not refuse because of the ledger")
	}
	if w.count() != 1 {
		t.Fatalf("warnings = %d (%v), want exactly 1 per failure streak", w.count(), w.lines)
	}
	if !containsAll(w.lines[0], []string{"connection refused", "local accounting"}) {
		t.Fatalf("warning %q must carry the cause and say accounting stays local",
			w.lines[0])
	}
	mem.setFailing(false)
	if err := l.flush(ctx); err != nil {
		t.Fatalf("flush after recovery: %v", err)
	}
	if got := mem.get(ledgerToday(), "name:orders", "general"); got != 60 {
		t.Fatalf("recovered flush wrote %d, want 60", got)
	}
	// A new streak needs a flush with something to write: unchanged usage
	// does not reach the store at all.
	if _, _, err := general.Chat(ctx, "sys", "after recovery", 10); err != nil {
		t.Fatal(err)
	}
	mem.setFailing(true)
	_ = l.flush(ctx)
	if w.count() != 2 {
		t.Fatalf("warnings = %d, want a second warning for the new streak", w.count())
	}
}

// A ledger that was down at startup restores on its next successful tick.
func TestLLMUsageLedger_TickRetriesRestore(t *testing.T) {
	fleetLedgerRuntime(t, 10)
	general, _ := newFleetDBLLMClients("orders", true)
	mem := newMemUsageStore()
	mem.set(ledgerToday(), "name:orders", "general", 250)
	mem.loadFail = true
	l, _ := testLedger(mem, newLLMUsageKeys())
	ctx := context.Background()
	_ = l.restore(ctx)
	if general.TokensUsedToday() != 0 {
		t.Fatal("a failed restore applied usage")
	}
	mem.mu.Lock()
	mem.loadFail = false
	mem.mu.Unlock()
	l.tick(ctx)
	if got := general.TokensUsedToday(); got != 250 {
		t.Fatalf("client after the retried restore = %d, want 250", got)
	}
	l.tick(ctx)
	if got := general.TokensUsedToday(); got != 250 {
		t.Fatalf("a second tick restored again: %d, want 250", got)
	}
}

func TestLLMUsageLedger_StatusTimes(t *testing.T) {
	fleetLedgerRuntime(t, 10)
	l, _ := testLedger(newMemUsageStore(), newLLMUsageKeys())
	if _, flushed := l.status(); !flushed.IsZero() {
		t.Fatal("last flush must be zero before any flush")
	}
	start := time.Now()
	if err := l.restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := l.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	restored, flushed := l.status()
	if restored.Before(start) || flushed.Before(start) {
		t.Fatalf("status = (%v, %v), want both at or after %v", restored, flushed, start)
	}
	setActiveLLMUsageLedger(t, l)
	gotRestored, gotFlushed := llmClients.UsageLedgerStatus()
	if !gotRestored.Equal(restored) || !gotFlushed.Equal(flushed) {
		t.Fatalf("registry status = (%v, %v), want (%v, %v)",
			gotRestored, gotFlushed, restored, flushed)
	}
}

// The admin budget reset also clears today's ledger rows, so a restart
// does not undo the reset.
func TestLLMUsageLedger_ResetClearsToday(t *testing.T) {
	fleetLedgerRuntime(t, 40)
	general, _ := newFleetDBLLMClients("orders", true)
	ctx := context.Background()
	if _, _, err := general.Chat(ctx, "sys", "p", 10); err != nil {
		t.Fatal(err)
	}
	mem := newMemUsageStore()
	l, _ := testLedger(mem, newLLMUsageKeys())
	setActiveLLMUsageLedger(t, l)
	if err := l.flush(ctx); err != nil {
		t.Fatal(err)
	}
	llmClients.ResetBudgets()
	if len(mem.cleared) != 1 || !mem.cleared[0].Equal(ledgerToday()) {
		t.Fatalf("cleared days = %v, want today", mem.cleared)
	}
	if err := l.flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mem.get(ledgerToday(), "name:orders", "general"); got != 0 {
		t.Fatalf("ledger after reset = %d, want 0", got)
	}
	fleetLedgerRuntime(t, 40)
	rebuilt, _ := newFleetDBLLMClients("orders", true)
	l2, _ := testLedger(mem, newLLMUsageKeys())
	if err := l2.restore(ctx); err != nil {
		t.Fatal(err)
	}
	if rebuilt.TokensUsedToday() != 0 || fleetLedgerUsed("orders") != 0 {
		t.Fatal("a restart after a reset restored the pre-reset usage")
	}
}

func setActiveLLMUsageLedger(t *testing.T, l *llmUsageLedger) {
	t.Helper()
	previous := activeLLMUsageLedger.Swap(l)
	t.Cleanup(func() { activeLLMUsageLedger.Store(previous) })
}

func fleetLedgerUsed(database string) int { return fleetLLMBudget.Used(database) }

// Unchanged usage is not rewritten every tick.
func TestLLMUsageLedger_UnchangedUsageIsNotRewritten(t *testing.T) {
	fleetLedgerRuntime(t, 40)
	general, _ := newFleetDBLLMClients("orders", true)
	ctx := context.Background()
	if _, _, err := general.Chat(ctx, "sys", "p", 10); err != nil {
		t.Fatal(err)
	}
	mem := newMemUsageStore()
	l, _ := testLedger(mem, newLLMUsageKeys())
	for i := 0; i < 3; i++ {
		if err := l.flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if mem.flushes != 1 {
		t.Fatalf("store flushes = %d, want 1 (later ticks had nothing new)", mem.flushes)
	}
}

func TestLLMUsageLedger_FlushRacingSpends(t *testing.T) {
	fleetLedgerRuntime(t, 10)
	newFleetDBLLMClients("orders", true)
	mem := newMemUsageStore()
	l, _ := testLedger(mem, newLLMUsageKeys())
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); fleetLLMBudget.Spend("orders", 10) }()
		if i%10 == 0 {
			wg.Add(1)
			go func() { defer wg.Done(); _ = l.flush(ctx) }()
		}
	}
	wg.Wait()
	if err := l.flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mem.get(ledgerToday(), "name:orders", "fleet"); got != 500 {
		t.Fatalf("fleet row = %d, want 500", got)
	}
}
