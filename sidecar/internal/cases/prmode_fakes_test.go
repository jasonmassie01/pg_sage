package cases

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/codehost/codehosttest"
	"github.com/pg-sage/sidecar/internal/verify"
)

const prRepo = "acme/shop"

// memRecords is an in-memory PRStore and SourceFixRecorder: one lock per
// key, one source-fix row per finding.
type memRecords struct {
	mu          sync.Mutex
	locks       map[string]bool
	rows        map[int64]SourceFixPR
	cannot      string
	recordFails int
	recordCalls int
	states      []string
}

func newMemRecords() *memRecords {
	return &memRecords{locks: map[string]bool{}, rows: map[int64]SourceFixPR{}}
}

func (m *memRecords) Lock(_ context.Context, key string) (func(), bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks[key] {
		return nil, false, nil
	}
	m.locks[key] = true
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.locks, key)
	}, true, nil
}

func (m *memRecords) SourceFix(_ context.Context, id int64) (SourceFixPR, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[id]
	return row, ok, nil
}

func (m *memRecords) SetPRState(_ context.Context, id int64, state string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row := m.rows[id]
	row.PRState = state
	m.rows[id] = row
	m.states = append(m.states, state)
	return nil
}

func (m *memRecords) CanRecord(context.Context, int64) (string, error) {
	return m.cannot, nil
}

var errRecord = errors.New("source_fix write refused: connection reset")

func (m *memRecords) RecordOpened(_ context.Context, id int64, repo string,
	pr codehost.PullRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordCalls++
	if m.recordFails > 0 {
		m.recordFails--
		return errRecord
	}
	m.rows[id] = SourceFixPR{Origin: "pg_sage", Repo: repo, Number: pr.Number, URL: pr.URL,
		Stage: "pr_opened", PRState: "open"}
	return nil
}

func (m *memRecords) row(id int64) (SourceFixPR, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	return r, ok
}

func (m *memRecords) setRow(id int64, r SourceFixPR) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[id] = r
}

// stubModel answers every Chat with reply or err.
type stubModel struct {
	reply string
	err   error
	calls int
	user  string
}

func (s *stubModel) Chat(_ context.Context, _, user string) (string, error) {
	s.calls++
	s.user = user
	return s.reply, s.err
}

// stubSites answers every call-site lookup with site or err.
type stubSites struct {
	site PRCallSite
	err  error
}

func (s stubSites) CallSite(context.Context, int64) (PRCallSite, error) {
	return s.site, s.err
}

var prNow = time.Date(2026, 11, 17, 9, 30, 15, 0, time.UTC)

func migrationRepo() *codehosttest.Fake {
	host := codehosttest.New()
	host.AddFile(prRepo, "db/migrate/20260101000000_init.sql", "CREATE TABLE a (id int);")
	host.AddFile(prRepo, "db/migrate/20260301120000_add_orders.sql", "CREATE TABLE o();")
	host.AddFile(prRepo, "db/migrate/README", "notes")
	return host
}

func predicted(v float64) *float64 { return &v }

func migrationCandidate() PRCandidate {
	return PRCandidate{FindingID: 41, Database: "shop", Kind: PRKindMigration,
		Title: "Add a NOT VALID check on orders.total", RiskTier: "high",
		Why:         "orders.total accepts negative values the application never writes.",
		EvidenceIDs: []string{"finding:41", "decision:7"},
		MigrationSQL: "ALTER TABLE public.orders ADD CONSTRAINT orders_total_ck " +
			"CHECK (total >= 0) NOT VALID;",
		RollbackSQL: "ALTER TABLE public.orders DROP CONSTRAINT orders_total_ck;",
		Ladder: []verify.LadderRecord{{Tier: 1, Name: "plan_only", Method: "what_if",
			Result: verify.LadderPassed, Predicted: predicted(-35), Metric: "mean_exec_time",
			At: prNow}}}
}

func newTestPRMode(host codehost.Host, rec *memRecords, model PRModel,
	sites CallSiteFinder) *PRMode {
	m, err := NewPRMode(PRModeConfig{Repo: prRepo, MigrationsPath: "db/migrate"},
		PRModeDeps{Host: host, Store: rec, Recorder: rec, Model: model, CallSites: sites,
			Now: func() time.Time { return prNow }})
	if err != nil {
		panic(err)
	}
	return m
}

type codehostFake = codehosttest.Fake
