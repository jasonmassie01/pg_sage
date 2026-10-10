package decide

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
)

// D1: kill or freeze on the principal, the database or the fleet. Reads
// through existing read tools stay open to a frozen principal
// (agentguard.ToolAccess); the database and fleet flags stop changes.
func (d *Decider) d1(ctx context.Context, s *state) (Verdict, bool) {
	if !s.access.Allowed && s.access.Reason != agentguard.ReasonUnsponsored {
		detail := fmt.Sprintf("principal %s is %s", s.p.Name, s.p.Status)
		if s.p.FrozenReason != "" {
			detail += ": " + s.p.FrozenReason
		}
		return deny(s.access.Reason, "D1", detail, ""), true
	}
	if d.cfg.Freezes == nil || s.req.Capability == CapRead ||
		s.req.Kind == agentguard.ToolRead {
		return Verdict{}, false
	}
	frozen, why, err := d.cfg.Freezes.Frozen(ctx, s.req.PrincipalID, s.req.Database)
	if err != nil {
		return unavailable("D1", "reading the freeze flags", err), true
	}
	if frozen {
		return deny(agentguard.ReasonFrozen, "D1", "agent changes are frozen: "+why,
			"an admin lifts the freeze in pg_sage"), true
	}
	return Verdict{}, false
}

// D2: an active principal with an active sponsor. Existing propose tools
// of an unsponsored principal still queue at L2 (§6.4).
func (d *Decider) d2(_ context.Context, s *state) (Verdict, bool) {
	if s.access.Reason == agentguard.ReasonUnsponsored {
		return deny(agentguard.ReasonUnsponsored, "D2", fmt.Sprintf(
			"principal %s has no active sponsor", s.p.Name),
			"an admin assigns an active sponsor to the agent"), true
	}
	return Verdict{}, false
}

// D3: the database's evaluated environment is within the effective
// ceiling, min(principal, profile). The environment comes from the bound
// identity (envbind), never from the agent; unknown counts as prod.
func (d *Decider) d3(ctx context.Context, s *state) (Verdict, bool) {
	if s.req.Database == "" {
		return Verdict{}, false
	}
	s.env = d.environment(ctx, s.req.Database)
	ceiling := ceilingOf(envbind.Env(s.p.EnvCeiling))
	if s.profile != nil && s.profile.EnvCeiling != "" &&
		ceilingOf(s.profile.EnvCeiling).Rank() < ceiling.Rank() {
		ceiling = ceilingOf(s.profile.EnvCeiling)
	}
	if s.env.Within(ceiling) {
		return Verdict{}, false
	}
	return deny(ReasonEnvCeiling, "D3", fmt.Sprintf("database %s evaluates as %s, above "+
		"the agent's effective ceiling %s", s.req.Database, s.env, ceiling),
		"verify the database's environment label, or have two admins raise the "+
			"agent's env_ceiling"), true
}

func (d *Decider) environment(ctx context.Context, database string) envbind.Env {
	if d.cfg.Environments == nil {
		return envbind.EnvProd
	}
	b, err := d.cfg.Environments.Environment(ctx, database)
	if err != nil {
		return envbind.EnvProd
	}
	if env, perr := envbind.ParseEnv(string(b.Env)); perr == nil {
		return env
	}
	return envbind.EnvProd
}

// ceilingOf reads a ceiling; an unknown one is the narrowest, branch.
func ceilingOf(e envbind.Env) envbind.Env {
	if env, err := envbind.ParseEnv(string(e)); err == nil {
		return env
	}
	return envbind.EnvBranch
}

// D4: an agent_* capability is in the principal's profile. Existing
// propose tools are classed by §6.2.6 and capped at L2 instead.
func (d *Decider) d4(_ context.Context, s *state) (Verdict, bool) {
	if s.req.Kind != agentguard.ToolAgent {
		return Verdict{}, false
	}
	fix := "an admin adds the class to the agent's profile (two people)"
	switch {
	case !s.req.Capability.Valid():
		return deny(ReasonCapability, "D4", fmt.Sprintf("%q is not a capability class",
			s.req.Capability), ""), true
	case s.profile == nil:
		return deny(ReasonCapability, "D4", fmt.Sprintf("profile %q is not configured",
			s.p.Profile), "configure agents.profiles."+s.p.Profile), true
	case !s.profile.Allows(s.req.Capability):
		return deny(ReasonCapability, "D4", fmt.Sprintf("profile %q does not allow %s",
			s.p.Profile, s.req.Capability), fix), true
	}
	return Verdict{}, false
}

// D5: objects resolve and their classification and grants admit the
// request (the checker is the broker's).
func (d *Decider) d5(ctx context.Context, s *state) (Verdict, bool) {
	if len(s.req.Objects) == 0 {
		return Verdict{}, false
	}
	if d.cfg.Objects == nil {
		return deny(ReasonClassification, "D5", "no classification checker is "+
			"configured, so no object can be checked", ""), true
	}
	err := d.cfg.Objects.CheckObjects(ctx, s.p, s.req.Database, s.env, s.req.Objects)
	if err == nil {
		return Verdict{}, false
	}
	if denied, ok := agentguard.IsDenied(err); ok {
		detail := denied.Detail
		if detail == "" {
			detail = string(denied.Reason)
		}
		return deny(ReasonClassification, "D5", detail, denied.Fix), true
	}
	return unavailable("D5", "checking objects", err), true
}

// D6: the change freeze is off.
func (d *Decider) d6(ctx context.Context, s *state) (Verdict, bool) {
	if d.cfg.ChangeFreeze == nil || !s.req.Capability.Mutates() {
		return Verdict{}, false
	}
	frozen, err := d.cfg.ChangeFreeze(ctx, s.req.Database)
	if err != nil {
		return unavailable("D6", "reading the change freeze", err), true
	}
	if frozen {
		return deny(ReasonChangeFreeze, "D6", "a change freeze is on for "+
			s.req.Database, "wait for the operator to lift the change freeze"), true
	}
	return Verdict{}, false
}

// D7: writes and DDL in prod need PITR; a drill older than
// require_restore_drill_days caps the level at L2.
func (d *Decider) d7(ctx context.Context, s *state) (Verdict, bool) {
	if s.env != envbind.EnvProd || !(s.req.Capability.Writes() || s.req.Capability.DDL()) {
		return Verdict{}, false
	}
	fix := "enable point-in-time recovery (WAL archiving or the provider's PITR)"
	if d.cfg.Recovery == nil {
		return deny(ReasonNoPITR, "D7", "recovery posture is unknown for "+
			s.req.Database, fix), true
	}
	pitr, drill, err := d.cfg.Recovery.Recovery(ctx, s.req.Database)
	if err != nil {
		return unavailable("D7", "reading recovery posture", err), true
	}
	if !pitr {
		return deny(ReasonNoPITR, "D7", s.req.Database+" has no PITR posture", fix), true
	}
	s.level = d.drillCap(drill, s.level)
	return Verdict{}, false
}

func (d *Decider) drillCap(last time.Time, level int) int {
	maxAge := time.Duration(d.cfg.RestoreDrillDays) * 24 * time.Hour
	if last.IsZero() || d.cfg.Now().Sub(last) > maxAge {
		return min(level, 2)
	}
	return level
}
