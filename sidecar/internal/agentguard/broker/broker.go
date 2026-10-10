package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/explain"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// Broker serves agent_query, the attribution view and agent_whoami. It is
// safe for concurrent use.
type Broker struct {
	cfg   Config
	deps  Deps
	pools *pools
	proof explain.ReadProofOptions
}

// New returns a broker over cfg and deps.
func New(cfg Config, deps Deps) (*Broker, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := deps.validate(); err != nil {
		return nil, err
	}
	if deps.Log == nil {
		deps.Log = func(string, string, ...any) {}
	}
	return &Broker{cfg: cfg, deps: deps, pools: newPools(cfg),
		proof: explain.ReadProofOptions{AllowRLS: true, DenySecurityDefiner: true,
			DenyFunction: deniedFunction, AllowCatalogRelation: catalogAllowed}}, nil
}

// Close closes every broker connection.
func (b *Broker) Close() { b.pools.evict("") }

// Evict closes a principal's broker connections (a freeze or kill).
func (b *Broker) Evict(principalID string) { b.pools.evict(principalID) }

// call is one agent_query in flight.
type call struct {
	b       *Broker
	id      agentguard.Identity
	t       Target
	req     Request
	params  [][]byte
	maxRows int
	rec     AuditRecord
}

// Query runs one agent read. Refusals and database errors are results;
// an error is invalid arguments, a database outside the token, an unknown
// database, or an unavailable dependency (then no rows are returned).
func (b *Broker) Query(ctx context.Context, req Request) (Result, error) {
	maxRows, params, err := validateRequest(b.cfg, req)
	if err != nil {
		return Result{}, err
	}
	id, ok := agentguard.IdentityFromContext(ctx)
	if !ok {
		return blocked(string(agentguard.ReasonUnsponsored),
			"agent_query runs as an agent principal; this caller has none",
			"use an agent token minted for a principal, or set mcp.stdio_principal"), nil
	}
	if !id.MayUseDatabase(req.Database) {
		return Result{}, fmt.Errorf("%w: %s", ErrNotPermitted, req.Database)
	}
	t, err := b.deps.Targets.Target(ctx, req.Database)
	if err != nil {
		return Result{}, err
	}
	if t.DatabaseID == "" {
		return blocked(ReasonDatabaseUnbound, "the database has no bound identity yet, so "+
			"its reads cannot be attributed", "wait for pg_sage to bind the database "+
			"(its first SRE cycle) and retry"), nil
	}
	c := &call{b: b, id: id, t: t, req: req, params: params, maxRows: maxRows,
		rec: AuditRecord{PrincipalID: id.Principal.ID, TaskID: id.TaskID,
			EnvelopeHash: rawHash(req.SQL)}}
	res, err := c.run(ctx)
	return c.finish(ctx, res, err)
}

func rawHash(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(sum[:])
}

// finish records the audit row; without it no rows leave.
func (c *call) finish(ctx context.Context, res Result, err error) (Result, error) {
	if err != nil {
		return Result{}, err
	}
	c.rec.Verdict = res.Verdict
	switch {
	case res.Verdict == VerdictBlocked:
		c.rec.Reason = res.ReasonCode
	case res.Status == StatusFailed:
		c.rec.Reason = "sqlstate_" + res.SQLState
	default:
		n := res.RowCount
		c.rec.RowCount = &n
	}
	if res.EnvelopeHash != "" {
		c.rec.EnvelopeHash = res.EnvelopeHash
	}
	if err := c.b.deps.Audit.Record(ctx, c.t, c.rec); err != nil {
		c.b.deps.Log("ERROR", "agent_query by %s on %s returned nothing: %v",
			c.id.Principal.ID, c.t.Name, err)
		if errors.Is(err, ErrUnavailable) {
			return Result{}, err
		}
		return Result{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return res, nil
}

// run screens, parses, decides, logs in and executes.
func (c *call) run(ctx context.Context) (Result, error) {
	if r, at, found := forbiddenRune(c.req.SQL); found {
		return blocked(ReasonForbiddenCharacters, fmt.Sprintf("the statement contains "+
			"%U at byte %d (a hidden or control character)", r, at),
			"remove invisible and bidi control characters"), nil
	}
	read, err := sqlast.InspectBrokeredRead(c.req.SQL)
	switch {
	case errors.Is(err, sqlast.ErrUnavailable):
		return blocked(ReasonParserUnavailable, "this pg_sage build cannot parse SQL, so "+
			"no statement can be verified", "run a pg_sage build with the SQL parser"), nil
	case err != nil:
		return blocked(ReasonUnsupportedShape, err.Error(),
			"send exactly one SELECT statement (no writes, SET, locking or INTO)"), nil
	case read.Params != len(c.params):
		return Result{}, invalidf("the statement uses %d parameters, %d given", read.Params,
			len(c.params))
	}
	c.rec.Fingerprint = read.Fingerprint
	envelope := envelopeHash(c.id.Principal.ID, c.t.DatabaseID, read.Canonical, c.params)
	c.rec.EnvelopeHash = envelope
	res, err := c.decideAndRun(ctx, read)
	res.EnvelopeHash = envelope
	return res, err
}

// decideAndRun asks the gate (kind read, with the touched objects), then
// opens the broker login and executes.
func (c *call) decideAndRun(ctx context.Context, read sqlast.BrokeredRead) (Result, error) {
	rels, err := resolveRelations(ctx, c.t.Pool, read.Query, c.b.cfg.SearchPath)
	if err != nil {
		return Result{}, err
	}
	v := c.b.deps.Decider.Decide(ctx, decide.Request{PrincipalID: c.id.Principal.ID,
		Tool: "agent_query", Kind: agentguard.ToolAgent, Capability: decide.CapRead,
		Database: c.t.Name, Objects: objectsOf(rels), TaskID: c.id.TaskID})
	c.rec.Step = v.Step
	if !v.Allowed {
		return blocked(string(v.Reason), v.Detail, v.Fix), nil
	}
	p := v.Principal
	if p.ID == "" {
		p = c.id.Principal
	}
	role, password, err := c.b.deps.Logins.BrokerLogin(ctx, p.ID, c.t.ClusterKey)
	if res, failed, err := loginRefusal(err); failed {
		return res, err
	}
	c.rec.BrokerRole = role
	return c.execute(ctx, p, role, password, read, rels, v)
}

// loginRefusal maps a broker credential failure.
func loginRefusal(err error) (Result, bool, error) {
	var denied *agentguard.DeniedError
	switch {
	case err == nil:
		return Result{}, false, nil
	case errors.Is(err, agentguard.ErrNotFound):
		return blocked(ReasonNoRole, "the agent has no broker login on this cluster",
			"an operator provisions the agent's roles (guard_role_ensure)"), true, nil
	case errors.Is(err, agentguard.ErrEncryptionKeyRequired):
		return blocked(ReasonEncryptionKey, "broker credentials need encryption_key",
			"set encryption_key (SAGE_ENCRYPTION_KEY)"), true, nil
	case errors.As(err, &denied):
		return blocked(string(denied.Reason), denied.Detail, denied.Fix), true, nil
	}
	return Result{}, true, fmt.Errorf("%w: reading the broker credential: %v",
		ErrUnavailable, err)
}
