package grants

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
)

// Bounds of one grant request.
const (
	// MinDuration is the shortest grant: the reconciler runs every minute.
	MinDuration = time.Minute
	// MaxObjects bounds the objects of one request.
	MaxObjects = 50
	// MaxColumns bounds the columns asked for on one object.
	MaxColumns = 1600
)

// Config is the grant settings (agents.capabilities, agents.unmask).
type Config struct {
	// MaxDuration is agents.capabilities.max_duration_minutes.
	MaxDuration time.Duration
	// Unmasked reports an agents.unmask entry for a pii column; nil
	// unmasks nothing.
	Unmasked func(principalID string, col classify.Column) bool
}

// Manager runs guard_grant and guard_revoke.
type Manager struct {
	principals PrincipalSource
	cfg        Config
}

// NewManager returns a manager over the core's principal store.
func NewManager(principals PrincipalSource, cfg Config) (*Manager, error) {
	if principals == nil {
		return nil, fmt.Errorf("%w: grants need the principal store", agentguard.ErrInvalid)
	}
	if cfg.MaxDuration < MinDuration {
		return nil, fmt.Errorf("%w: the maximum grant duration must be at least %v",
			agentguard.ErrInvalid, MinDuration)
	}
	return &Manager{principals: principals, cfg: cfg}, nil
}

// MaxDuration is the longest grant the manager allows.
func (m *Manager) MaxDuration() time.Duration { return m.cfg.MaxDuration }

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{agentguard.ErrInvalid}, args...)...)
}

func deny(reason agentguard.Reason, fix, format string, args ...any) error {
	return &agentguard.DeniedError{Reason: reason, Detail: fmt.Sprintf(format, args...),
		Fix: fix}
}

func (t Target) validate() error {
	switch {
	case t.Pool == nil:
		return invalidf("database %q has no pg_sage connection", t.Name)
	case t.Executor == nil:
		return invalidf("database %q has no executor", t.Name)
	case !uuidLike(t.ID):
		return invalidf("database %q has no SRE binding id; grants are recorded per "+
			"bound database", t.Name)
	}
	return nil
}

func uuidLike(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		dash := i == 8 || i == 13 || i == 18 || i == 23
		hex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if dash != (r == '-') || (!dash && !hex) {
			return false
		}
	}
	return true
}

func (r GrantRequest) validate(maxDuration time.Duration) error {
	switch {
	case !agentguard.ValidID(r.PrincipalID):
		return fmt.Errorf("%w: principal %q", agentguard.ErrNotFound, r.PrincipalID)
	case r.Approval.ApprovedBy <= 0:
		return agentguard.ErrApprovalRequired
	case r.Duration < MinDuration || r.Duration > maxDuration:
		return invalidf("duration must be %v to %v, got %v", MinDuration, maxDuration,
			r.Duration)
	case len(r.Objects) == 0 || len(r.Objects) > MaxObjects:
		return invalidf("a grant names 1 to %d objects", MaxObjects)
	case len(r.Reason) > 2000:
		return invalidf("reason is longer than 2000 characters")
	}
	if err := r.Target.validate(); err != nil {
		return err
	}
	for _, o := range r.Objects {
		if err := o.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (o ObjectRequest) validate() error {
	if _, _, err := parseObject(o.Object); err != nil {
		return err
	}
	if len(o.Columns) > MaxColumns {
		return invalidf("%s: at most %d columns", o.Object, MaxColumns)
	}
	seen := map[string]bool{}
	for _, c := range o.Columns {
		if c == "" || len(c) > 63 || strings.ContainsRune(c, 0) || seen[c] {
			return invalidf("%s: column names must be unique, 1-63 characters", o.Object)
		}
		seen[c] = true
	}
	return nil
}

// checkPrincipal applies D1-D4 for an operator's grant: the principal is
// active and sponsored (ToolAccess for agent_* tools), its ceiling covers
// the database's evaluated environment, and the capability is read.
func (m *Manager) checkPrincipal(ctx context.Context, r GrantRequest) (
	agentguard.Principal, error) {
	p, err := m.principals.Get(ctx, r.PrincipalID)
	if err != nil {
		return p, err
	}
	if a := agentguard.ToolAccess(p, agentguard.ToolAgent); !a.Allowed {
		return p, deny(a.Reason, "", "principal %s cannot hold grants", p.Name)
	}
	if !p.EnvCeiling.Valid() {
		return p, deny(decide.ReasonEnvCeiling, "", "principal %s has no valid ceiling",
			p.Name)
	}
	if effectiveEnv(r.Target.Env).Rank() > envbind.Env(p.EnvCeiling).Rank() {
		return p, deny(decide.ReasonEnvCeiling, "", "database %s evaluates as %s, above "+
			"the ceiling %s of %s", r.Target.Name, effectiveEnv(r.Target.Env),
			p.EnvCeiling, p.Name)
	}
	if r.Capability != CapabilityRead {
		if !decide.Capability(r.Capability).Valid() {
			return p, invalidf("capability %q is not a capability class", r.Capability)
		}
		return p, deny(decide.ReasonCapability, "", "G1 grants only the read capability; "+
			"%s comes with the brokered write path", r.Capability)
	}
	return p, m.checkRoles(ctx, p)
}

// effectiveEnv is the environment a grant is treated as: anything that is
// not a known label is prod.
func effectiveEnv(e envbind.Env) envbind.Env {
	if _, err := envbind.ParseEnv(string(e)); err != nil {
		return envbind.EnvProd
	}
	return e
}

// checkRoles requires the principal's broker role registered and active.
func (m *Manager) checkRoles(ctx context.Context, p agentguard.Principal) error {
	roles, err := m.principals.ClusterRoles(ctx, p.ID)
	if err != nil {
		return err
	}
	for _, r := range roles {
		if r.BrokerRole == p.BrokerRole() && r.Status == "active" {
			return nil
		}
	}
	return deny(ReasonNoRoles, "", "principal %s has no active broker role; approve "+
		"guard_role_ensure for its cluster first", p.Name)
}
