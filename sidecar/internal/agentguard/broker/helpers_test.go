package broker

import (
	"context"
	"errors"
	"sync"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
)

// fakeTargets resolves database names to fixed targets.
type fakeTargets map[string]Target

func (f fakeTargets) Target(_ context.Context, name string) (Target, error) {
	t, ok := f[name]
	if !ok {
		return Target{}, ErrUnknownDatabase
	}
	return t, nil
}

// fakeLogins hands out one broker credential per principal, or an error.
type fakeLogins struct {
	mu    sync.Mutex
	role  string
	pw    string
	err   error
	calls int
}

func (f *fakeLogins) BrokerLogin(_ context.Context, _, _ string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.role, f.pw, f.err
}

func (f *fakeLogins) set(pw string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pw, f.err = pw, err
}

// fakeDecider answers every request with one verdict and records requests.
type fakeDecider struct {
	mu       sync.Mutex
	verdict  decide.Verdict
	requests []decide.Request
}

func allowIn(env envbind.Env, p agentguard.Principal) *fakeDecider {
	return &fakeDecider{verdict: decide.Verdict{Allowed: true, MaxLevel: 3, Env: env,
		Capability: decide.CapRead, Principal: p}}
}

func (f *fakeDecider) Decide(_ context.Context, req decide.Request) decide.Verdict {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	return f.verdict
}

func (f *fakeDecider) last() decide.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return decide.Request{}
	}
	return f.requests[len(f.requests)-1]
}

// noClasses classifies nothing.
type noClasses struct{}

func (noClasses) Lookup(_ context.Context, _ Target, relid uint32) (classify.RelationClasses,
	error) {
	return classify.RelationClasses{RelID: relid, Columns: map[int16]classify.Classification{}},
		nil
}

// memAudit keeps audit records in memory; fail makes Record fail.
type memAudit struct {
	mu      sync.Mutex
	records []AuditRecord
	fail    error
}

func (m *memAudit) Record(_ context.Context, _ Target, rec AuditRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.records = append(m.records, rec)
	return nil
}

func (m *memAudit) all() []AuditRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]AuditRecord(nil), m.records...)
}

var errAuditDown = errors.New("audit table unreachable")

func testPrincipal() agentguard.Principal {
	sponsor := 1
	return agentguard.Principal{ID: "agp_abcdefghijklmnopqrst", Name: "bq-bot",
		SponsorUserID: &sponsor, SponsorActive: true, Profile: "readonly-analyst",
		EnvCeiling: agentguard.Env("prod"), Status: agentguard.StatusActive}
}

func withAgent(ctx context.Context, p agentguard.Principal, dbs []string) context.Context {
	return agentguard.WithIdentity(ctx, agentguard.Identity{Principal: p, TokenID: "tok",
		Databases: dbs, TaskID: "task-1"})
}

func fakeDeps() Deps {
	p := testPrincipal()
	return Deps{Targets: fakeTargets{}, Logins: &fakeLogins{}, Decider: allowIn("prod", p),
		Classes: noClasses{}, Audit: &memAudit{}}
}
