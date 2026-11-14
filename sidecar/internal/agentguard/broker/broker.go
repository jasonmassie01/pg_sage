package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"

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
	parse parser
}

// parser is the SQL parser the broker verifies statements with: the C
// parser, or none on a build without cgo (every statement is then
// refused). Tests on such builds substitute a scripted one to reach the
// execution path; nothing else replaces it.
type parser struct {
	brokered func(sql string) (sqlast.BrokeredRead, error)
	read     func(sql string) (sqlast.ReadQuery, error)
}

func cParser() parser {
	return parser{brokered: sqlast.InspectBrokeredRead, read: sqlast.InspectReadQuery}
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
			DenyFunction: deniedFunction, AllowCatalogRelation: catalogAllowed},
		parse: cParser()}, nil
}

// Close closes every broker connection.
func (b *Broker) Close() { b.pools.evict("") }

// Evict closes a principal's broker connections (a freeze or kill).
func (b *Broker) Evict(principalID string) { b.pools.evict(principalID) }

// roleNamer lets a Logins source name a broker role other than the core's
// sage_agentb_<id10> (AgentSafetyBench's fixture role).
type roleNamer interface {
	BrokerRoleName(principalID string) string
}

// roleOf is the broker role the principal logs in as.
func (b *Broker) roleOf(principalID string) string {
	if n, ok := b.deps.Logins.(roleNamer); ok {
		return n.BrokerRoleName(principalID)
	}
	return agentguard.BrokerRoleName(principalID)
}

// call is one agent_query, or one agent explain_query, in flight.
type call struct {
	b       *Broker
	id      agentguard.Identity
	t       Target
	req     Request
	params  [][]byte
	maxRows int
	rec     AuditRecord
	// path is the profile schemas of the search path (grantedSchemas).
	path []string
	// tool is the MCP tool the gate decides for.
	tool string
	// explain plans the statement instead of fetching its rows; planJSON
	// is the plan, and prepared marks the statement to deallocate.
	explain  bool
	planJSON []byte
	prepared bool
}

// Query runs one agent read. Refusals and database errors are results;
// an error is invalid arguments, a database outside the token, an unknown
// database, or an unavailable dependency (then no rows are returned).
func (b *Broker) Query(ctx context.Context, req Request) (Result, error) {
	maxRows, params, err := validateRequest(b.cfg, req)
	if err != nil {
		return Result{}, err
	}
	c, refused, err := b.open(ctx, req.Database, req.SQL, "agent_query")
	if c == nil {
		return refused, err
	}
	c.req, c.params, c.maxRows = req, params, maxRows
	res, err := c.run(ctx)
	return c.finish(ctx, res, err)
}

// open starts a call: the caller's agent identity, the token's databases
// and the target's bound id. A nil call comes with a refusal or an error.
func (b *Broker) open(ctx context.Context, database, sql, tool string) (*call, Result,
	error) {
	id, ok := agentguard.IdentityFromContext(ctx)
	if !ok {
		return nil, blocked(string(agentguard.ReasonUnsponsored),
			tool+" runs as an agent principal; this caller has none",
			"use an agent token minted for a principal, or set mcp.stdio_principal"), nil
	}
	if !id.MayUseDatabase(database) {
		return nil, Result{}, fmt.Errorf("%w: %s", ErrNotPermitted, database)
	}
	t, err := b.deps.Targets.Target(ctx, database)
	if err != nil {
		return nil, Result{}, err
	}
	if t.DatabaseID == "" {
		return nil, blocked(ReasonDatabaseUnbound, "the database has no bound identity "+
			"yet, so its reads cannot be attributed", "wait for pg_sage to bind the "+
			"database (its first SRE cycle) and retry"), nil
	}
	return &call{b: b, id: id, t: t, tool: tool, rec: AuditRecord{PrincipalID: id.Principal.ID,
		TaskID: id.TaskID, EnvelopeHash: rawHash(sql)}}, Result{}, nil
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
	case c.explain:
		// A plan returns no rows: row_count stays NULL.
	default:
		n := res.RowCount
		c.rec.RowCount = &n
	}
	if res.EnvelopeHash != "" {
		c.rec.EnvelopeHash = res.EnvelopeHash
	}
	if err := c.b.deps.Audit.Record(ctx, c.t, c.rec); err != nil {
		c.b.deps.Log("ERROR", "%s by %s on %s returned nothing: %v", c.tool,
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
	read, err := c.b.parse.brokered(c.req.SQL)
	switch {
	case errors.Is(err, sqlast.ErrUnavailable):
		return blocked(ReasonParserUnavailable, "this pg_sage build cannot parse SQL, so "+
			"no statement can be verified", "run a pg_sage build with the SQL parser"), nil
	case err != nil:
		return blocked(ReasonUnsupportedShape, err.Error(),
			"send exactly one SELECT statement (no writes, SET, locking or INTO)"), nil
	case !c.paramsFit(read.Params):
		return Result{}, invalidf("the statement uses %d parameters, %d given", read.Params,
			len(c.params))
	}
	c.rec.Fingerprint = read.Fingerprint
	envelope := envelopeHash(c.id.Principal.ID, c.t.DatabaseID, c.envelopeStatement(read),
		c.params)
	c.rec.EnvelopeHash = envelope
	res, err := c.decideAndRun(ctx, read)
	res.EnvelopeHash = envelope
	return res, err
}

// decideAndRun asks the gate (kind read, with the touched objects), then
// opens the broker login and executes.
func (c *call) decideAndRun(ctx context.Context, read sqlast.BrokeredRead) (Result, error) {
	path, err := grantedSchemas(ctx, c.t.Pool, c.b.roleOf(c.id.Principal.ID))
	if err != nil {
		return Result{}, err
	}
	c.path = path
	rels, err := resolveRelations(ctx, c.t.Pool, read.Query, c.path)
	if err != nil {
		return Result{}, err
	}
	v := c.b.deps.Decider.Decide(ctx, decide.Request{PrincipalID: c.id.Principal.ID,
		Tool: c.tool, Kind: agentguard.ToolAgent, Capability: decide.CapRead,
		Database: c.t.Name, Objects: objectsOf(rels), TaskID: c.id.TaskID})
	c.rec.Step = v.Step
	if !v.Allowed {
		res := blocked(string(v.Reason), v.Detail, v.Fix)
		if v.Park {
			res.RetryAfterSeconds = int(math.Ceil(v.RetryAfter.Seconds()))
		}
		return res, nil
	}
	p := v.Principal
	if p.ID == "" {
		p = c.id.Principal
	}
	role, password, err := c.b.deps.Logins.BrokerLogin(ctx, p.ID, c.t.ClusterKey)
	if res, failed, err := loginRefusal(err); failed {
		return res, err
	}
	if role != c.b.roleOf(p.ID) {
		return Result{}, fmt.Errorf("%w: the broker credential names role %q, not the "+
			"agent's broker role", ErrUnavailable, role)
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
