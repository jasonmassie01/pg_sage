package config

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/managedparam"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/provider"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/sanitize"
	"github.com/pg-sage/sidecar/internal/verify"
)

// noControl is lab-01's marker every global-setting prediction carries:
// there is no control group, so the verdict is an interrupted time series
// against the seasonal baseline.
var noControl = verify.MethodITS + ": " + verify.ReasonGlobal +
	"; interrupted time series against the seasonal baseline"

// finding is one rule's contradiction: the setting, the value the rule
// computed (already clamped), what it cites and why. advice marks a rule
// whose setting pg_sage never proposes as a change (io_method, or a
// database-scope setting the pack cannot name the database for).
type finding struct {
	kind, setting, value string
	evidence             []string
	reason               string
	metric, direction    string
	quantities           map[string]float64
	advice               bool
}

// builder collects the candidates of one review. restart is the setting
// holding the review's one restart-bound slot (the plan-02 cap).
type builder struct {
	v       view
	cov     *candidate.Coverage
	out     []candidate.Candidate
	restart string
}

func newBuilder(v view, cov *candidate.Coverage) *builder {
	return &builder{v: v, cov: cov}
}

// emit turns a finding into its candidate: advice, an alter_system_guc
// step (reloadable, self-managed), or an operator step (restart-bound, or
// a managed service, through managedparam). A second restart-bound step
// goes to the next review, noted in the coverage.
func (b *builder) emit(f finding) {
	c := candidate.Candidate{Generator: Name, Dimension: Dimension, Kind: f.kind,
		Target: f.setting, EvidenceIDs: f.evidence, Reason: f.reason,
		Approval:       candidate.ApprovalRequired,
		ApprovalReason: "a global setting changes every workload on the server",
		Prediction: candidate.Prediction{Metric: f.metric, Direction: f.direction,
			Tier: candidate.TierNone, Source: "rule " + f.kind + "; " + noControl,
			Quantities: f.quantities}}
	restart := pgconf.RequiresRestart(f.setting)
	switch {
	case f.advice:
		b.advise(&c, f, "")
	case selfManaged(b.v.platform) && !restart:
		b.alterSystem(&c, f)
	case selfManaged(b.v.platform):
		b.selfOperator(&c, f)
	default:
		b.managed(&c, f)
	}
	if c.ActionType == candidate.ActionOperator && restart {
		if b.restart != "" {
			b.cov.Note("%s = %s goes to the next review: at most one restart-bound step "+
				"per review, and %s holds it", f.setting, f.value, b.restart)
			return
		}
		b.restart = f.setting
	}
	c.ID = candidate.NewID(c.Generator, c.Kind, c.Target, c.IDBody())
	b.out = append(b.out, c)
}

// selfManaged is a server whose settings change with ALTER SYSTEM.
func selfManaged(platform string) bool {
	p, ok := provider.Lookup(platform)
	return ok && p.ConfigMechanism == provider.ConfigAlterSystem
}

func (b *builder) alterSystem(c *candidate.Candidate, f finding) {
	c.ActionType, c.Reversible = "alter_system_guc", true
	c.ForwardSQL = "ALTER SYSTEM SET " + f.setting + " = " + sanitize.QuoteLiteral(f.value)
	c.RollbackSQL = b.priorSQL(f.setting)
}

// priorSQL restores the setting as the pack saw it: RESET from the
// built-in default, the prior value otherwise. The executor captures the
// live prior again when the step runs (its rollback is the one it keeps).
func (b *builder) priorSQL(name string) string {
	s := b.v.settings[name]
	if s.Source == "default" || s.Source == "" {
		return "ALTER SYSTEM RESET " + name
	}
	return "ALTER SYSTEM SET " + name + " = " + sanitize.QuoteLiteral(shown(s))
}

// selfOperator is a restart-bound setting on a self-managed server: the
// operator runs ALTER SYSTEM and restarts; pg_sage watches it land.
func (b *builder) selfOperator(c *candidate.Candidate, f finding) {
	c.ActionType = candidate.ActionOperator
	const then = "; then restart PostgreSQL (the setting takes effect only after a restart)"
	c.Operator = &candidate.Operator{Setting: f.setting, Value: f.value,
		Command: "ALTER SYSTEM SET " + f.setting + " = " + sanitize.QuoteLiteral(f.value) +
			then,
		Revert: b.priorSQL(f.setting) + then, RebootRequired: true}
}

// managed builds managedparam's proposal for the platform. An intent or
// proposal managedparam refuses (an unsupported provider, a value it
// cannot carry) makes the candidate advice with that reason.
func (b *builder) managed(c *candidate.Candidate, f finding) {
	in, err := managedparam.NewIntent(b.v.platform, f.setting, f.value, f.reason)
	if err != nil {
		b.advise(c, f, err.Error())
		return
	}
	s := b.v.settings[f.setting]
	p, err := managedparam.Build(in, managedparam.Target{Provider: in.Provider},
		&collector.PGSetting{Name: s.Name, Setting: s.Value, Unit: s.Unit, Source: s.Source})
	if err != nil {
		b.advise(c, f, err.Error())
		return
	}
	c.ActionType = candidate.ActionOperator
	c.Operator = &candidate.Operator{Command: p.CLI, Revert: p.Rollback.CLI,
		Setting: f.setting, Value: f.value, Provider: p.Provider, Mechanism: p.Mechanism,
		RebootRequired: p.RebootRequired || pgconf.RequiresRestart(f.setting)}
}

// advise makes the candidate text for a person, naming the value and, when
// pg_sage cannot describe the change here, why.
func (b *builder) advise(c *candidate.Candidate, f finding, why string) {
	c.ActionType = candidate.ActionAdvice
	if why != "" {
		c.Reason = fmt.Sprintf("%s %s = %s is recommended, but pg_sage cannot describe the "+
			"change on platform %q: %s", f.reason, f.setting, f.value, b.v.platform, why)
	}
}

// shown is a setting's value as SHOW prints it.
func shown(s workpack.SettingFact) string {
	d := docFor(s)
	n, err := pgconf.ParseValue(s.Value, d)
	switch {
	case err != nil:
		return s.Value
	case d.Unit == "bytes":
		return renderBytes(n)
	case d.Unit == "ms":
		return renderMillis(n)
	}
	return s.Value
}
