package probes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ReadPolicy is trusted runtime configuration, never model/request authority.
// Nil preserves the existing self-managed behavior. A non-nil policy is pinned
// in an investigation's created event and cannot change silently on resume.
type ReadPolicy struct {
	Version  int    `json:"version"`
	Scope    string `json:"scope"`
	Database string `json:"database"`
	Binding  string `json:"binding"`
	Allowed  []ID   `json:"allowed"`
	QueryID  int64  `json:"queryid,omitempty"`
}

const ReasonPolicyDenied = "policy_scope_denied"

// PoolBinding pins the connection target without exporting host/credentials.
func PoolBinding(pool *pgxpool.Pool) string {
	if pool == nil {
		return ""
	}
	c := pool.Config().ConnConfig
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d/%s", c.Host, c.Port, c.Database)))
	return hex.EncodeToString(h[:])
}

// SourceScope describes the observation, not the database used to connect.
// Unknown sources fail closed under an explicit policy.
func SourceScope(id ID) string {
	switch id {
	case LockGraph, LongTransactions, PreparedXacts, BackendIdentity,
		AutovacuumWraparound, VacuumProgress, PlanRegressions, SageActions,
		TempFileActivity, TempSpillStatements, WraparoundTablesProbe,
		SequenceRunwayProbe, RunwayTrendsProbe,
		StatDatabase, StatTables, StatStatements, RecoverySample, ChangeFeed,
		"explain_statement":
		return "database"
	case LakebasePlans, LakebaseLogs, LakebaseSessions, LakebaseChanges, LakebaseDatabase:
		return "database"
	case ApplicationSLOStatus:
		return "database"
	case LockChains, ConnectionSaturation, ReplicationLag, ReplicationSlots,
		WALCheckpoint, Archiver, CheckpointActivity, StandbyReplayState,
		LWLockWaits, XIDRunwayProbe, XminHorizon, WALRunwayProbe,
		ClusterDatabaseSizeProbe, SignalTarget, "custodian_freeze", "custodian_wal":
		return "cluster"
	case TempFileHolders, WALDirectoryProbe:
		return "filesystem"
	case SLOStatus, PoolerPools, AutovacuumCancellations:
		return "external"
	default:
		return "unknown"
	}
}

func (p *ReadPolicy) Clone() *ReadPolicy {
	if p == nil {
		return nil
	}
	c := *p
	if p.Allowed != nil {
		c.Allowed = append([]ID{}, p.Allowed...)
	}
	return &c
}

func (p *ReadPolicy) Validate() error {
	if p == nil {
		return nil
	}
	if p.Version != 1 || (p.Scope != "database" && p.Scope != "cluster") ||
		p.Database == "" || p.Binding == "" {
		return fmt.Errorf("invalid SRE read policy")
	}
	seen := map[ID]bool{}
	for _, id := range p.Allowed {
		if SourceScope(id) == "unknown" || seen[id] {
			return fmt.Errorf("invalid or repeated SRE source %q", id)
		}
		seen[id] = true
	}
	return nil
}

func (p *ReadPolicy) Hash() string {
	if p == nil {
		return "legacy"
	}
	c := p.Clone()
	slices.Sort(c.Allowed)
	b, _ := json.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (p *ReadPolicy) Allows(id ID) bool {
	if p == nil {
		return true
	}
	if p.Validate() != nil || SourceScope(id) == "unknown" {
		return false
	}
	if p.Scope == "database" && SourceScope(id) != "database" {
		return false
	}
	if p.Allowed != nil && !slices.Contains(p.Allowed, id) {
		return false
	}
	// A statement-bound policy cannot expose the other statements' counters.
	if p.QueryID != 0 && (id == StatStatements || id == TempSpillStatements) {
		return false
	}
	if p.QueryID != 0 && (id == LakebaseLogs || id == LakebaseChanges || id == LakebaseDatabase || id == ApplicationSLOStatus) {
		return false
	}
	return true
}

func PolicyDenied(id ID, reason string) Result {
	return Result{ProbeID: id, Version: "v1", ObservedAt: time.Now(),
		Status: StatusUnsupported, Reason: ReasonPolicyDenied, Error: reason}
}

type readPolicyKey struct{}

// WithReadPolicy narrows execution in addition to the runner's own policy.
func WithReadPolicy(ctx context.Context, p *ReadPolicy) context.Context {
	if p == nil {
		return ctx
	}
	copy := p.Clone()
	if previous := ContextReadPolicy(ctx); previous != nil {
		if previous.Database != copy.Database || previous.Binding != copy.Binding {
			copy.Version = 0 // a different binding can never be an intersection
		}
		if previous.Scope == "database" {
			copy.Scope = "database"
		}
		if previous.QueryID != 0 {
			if copy.QueryID != 0 && copy.QueryID != previous.QueryID {
				copy.Version = 0
			}
			copy.QueryID = previous.QueryID
		}
		copy.Allowed = []ID{}
		for _, id := range AllSourceIDs() {
			if p.Allows(id) && previous.Allows(id) {
				copy.Allowed = append(copy.Allowed, id)
			}
		}
	}
	return context.WithValue(ctx, readPolicyKey{}, copy)
}

func AllSourceIDs() []ID {
	return append(append(append(Catalog().IDs(), ActionRegistry().IDs()...), ChangeFeed,
		SLOStatus, ApplicationSLOStatus, PoolerPools, "explain_statement", "custodian_freeze", "custodian_wal"), HistoricalIDs()...)
}

func ContextReadPolicy(ctx context.Context) *ReadPolicy {
	p, _ := ctx.Value(readPolicyKey{}).(*ReadPolicy)
	return p
}

func CheckReadPolicy(ctx context.Context, p *ReadPolicy, id ID, args Args) *Result {
	for _, policy := range []*ReadPolicy{p, ContextReadPolicy(ctx)} {
		if !policy.Allows(id) {
			r := PolicyDenied(id, "source outside trusted read policy")
			return &r
		}
		if policy != nil && policy.QueryID != 0 && args.QueryID != policy.QueryID &&
			(id == PlanRegressions || id == "explain_statement" || id == LakebasePlans || id == LakebaseSessions) {
			r := PolicyDenied(id, "statement outside trusted read policy")
			return &r
		}
	}
	return nil
}

func effectiveDatabasePolicy(ctx context.Context, p *ReadPolicy) bool {
	return p != nil && p.Scope == "database" || ContextReadPolicy(ctx) != nil && ContextReadPolicy(ctx).Scope == "database"
}
