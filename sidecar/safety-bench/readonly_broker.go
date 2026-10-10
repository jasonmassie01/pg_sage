package safetybench

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/broker"
	classes "github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
)

// agentQueryDesign is the fourth read-only design (G1): pg_sage's own
// agent_query, the brokered read path. It runs each statement as a broker
// login role of its own (sb_agentb, the same grants as sb_readonly) in a
// session of its own, through the shipped broker with its parse
// allowlist, catalog proof, read-only transaction, own timeouts and column
// classes. The fixture database is evaluated as dev with
// sb_fixture.people.ssn classified pii, so the masking path is exercised.
// The gate's decision is fixed to "allowed" (a sponsored principal within
// its ceiling): the bench measures the read path, not the D-steps.
type agentQueryDesign struct {
	mu      sync.Mutex
	brokers map[*pgxpool.Pool]*broker.Broker
}

// agentQueryTimeout is the statement timeout the bench's broker enforces.
const agentQueryTimeout = 5 * time.Second

// benchPrincipal is the bench's agent.
var benchPrincipal = func() agentguard.Principal {
	sponsor := 1
	return agentguard.Principal{ID: "agp_safetybenchsafetybch", Name: "safety-bench",
		SponsorUserID: &sponsor, SponsorActive: true, Profile: "readonly-analyst",
		EnvCeiling: agentguard.EnvProd, Status: agentguard.StatusActive}
}()

func newAgentQueryDesign() *agentQueryDesign {
	return &agentQueryDesign{brokers: map[*pgxpool.Pool]*broker.Broker{}}
}

func (*agentQueryDesign) Name() string { return "agent_query" }

// Close closes the broker connections.
func (d *agentQueryDesign) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for pool, b := range d.brokers {
		b.Close()
		delete(d.brokers, pool)
	}
}

// Attempt maps the broker's answer to the bench's refusal classes: a
// blocked verdict is a refusal before execution, a failed execution is the
// server error, a result is an execution.
func (d *agentQueryDesign) Attempt(ctx context.Context, owner *pgxpool.Pool, sql string) error {
	res, err := d.run(ctx, owner, sql)
	switch {
	case err != nil:
		return fmt.Errorf("agent_query: %w", err)
	case res.Verdict == broker.VerdictBlocked:
		return beforeExecutionError{reason: "blocked " + res.ReasonCode + ": " + res.Detail}
	case res.Status == broker.StatusFailed:
		return &pgconn.PgError{Code: res.SQLState, Message: res.Message}
	}
	return nil
}

func (d *agentQueryDesign) run(ctx context.Context, owner *pgxpool.Pool, sql string) (
	broker.Result, error) {
	b, err := d.brokerFor(owner)
	if err != nil {
		return broker.Result{}, err
	}
	ctx = agentguard.WithIdentity(ctx, agentguard.Identity{Principal: benchPrincipal})
	return b.Query(ctx, broker.Request{Database: "bench", SQL: sql})
}

func (d *agentQueryDesign) brokerFor(owner *pgxpool.Pool) (*broker.Broker, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b, ok := d.brokers[owner]; ok {
		return b, nil
	}
	cfg := broker.DefaultConfig()
	cfg.StatementTimeout = agentQueryTimeout
	t := broker.Target{Name: "bench", DatabaseID: "00000000-0000-4000-8000-00000000be01",
		Pool: owner, ClusterKey: "bench", Env: envbind.EnvDev, Verified: true}
	b, err := broker.New(cfg, broker.Deps{Targets: benchTargets{t: t},
		Logins: benchLogins{}, Decider: benchDecider{}, Classes: benchClasses{},
		Audit: &benchAudit{}})
	if err != nil {
		return nil, err
	}
	d.brokers[owner] = b
	return b, nil
}

type benchTargets struct{ t broker.Target }

func (b benchTargets) Target(_ context.Context, name string) (broker.Target, error) {
	if name != b.t.Name {
		return broker.Target{}, broker.ErrUnknownDatabase
	}
	return b.t, nil
}

type benchLogins struct{}

func (benchLogins) BrokerLogin(context.Context, string, string) (string, string, error) {
	return agentQueryRole, agentQueryPassword, nil
}

type benchDecider struct{}

func (benchDecider) Decide(_ context.Context, req decide.Request) decide.Verdict {
	return decide.Verdict{Allowed: true, MaxLevel: 3, Env: envbind.EnvDev,
		Capability: req.Capability, Principal: benchPrincipal}
}

// benchClasses classifies sb_fixture.people.ssn as confirmed pii.
type benchClasses struct{}

func (benchClasses) Lookup(ctx context.Context, t broker.Target, relid uint32) (
	classes.RelationClasses, error) {
	rc := classes.RelationClasses{RelID: relid, Columns: map[int16]classes.Classification{}}
	var attnum int16
	err := t.Pool.QueryRow(ctx, `/* pg_sage safety_bench v1 */
		SELECT attnum FROM pg_catalog.pg_attribute
		WHERE attrelid = $1::oid AND attname = 'ssn'
		  AND attrelid = 'sb_fixture.people'::regclass`, relid).Scan(&attnum)
	if errors.Is(err, pgx.ErrNoRows) {
		return rc, nil // another relation: nothing classified
	}
	if err != nil {
		return rc, fmt.Errorf("classify fixture relation %d: %w", relid, err)
	}
	rc.Columns[attnum] = classes.Classification{Class: classes.ClassPII,
		Status: classes.StatusConfirmed, Live: true,
		Column: classes.Column{RelID: relid, AttNum: attnum, Name: "ssn"}}
	return rc, nil
}

// benchAudit counts audit rows; the bench database has no sage schema.
type benchAudit struct {
	mu sync.Mutex
	n  int
}

func (a *benchAudit) Record(context.Context, broker.Target, broker.AuditRecord) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n++
	return nil
}

// closeDesigns releases designs that hold connections.
func closeDesigns(designs []RODesign) {
	for _, d := range designs {
		if c, ok := d.(interface{ Close() }); ok {
			c.Close()
		}
	}
}
