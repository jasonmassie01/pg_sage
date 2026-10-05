package managedparam

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

const (
	MechanismParameterGroup = "parameter_group"
	MechanismDatabaseFlag   = "database_flag"
	ApplyImmediate          = "immediate"
	ApplyPendingReboot      = "pending-reboot"
)

// AutoApplyWithheld is why no managed change is applied by pg_sage in this
// release: applying one needs provider write permissions pg_sage does not
// ask for, a reboot or automatic restart is an outage, and the trust
// ledger has no managed-parameter action class whose promotion evidence
// could earn unattended execution.
const AutoApplyWithheld = "pg_sage never applies provider parameter changes in this " +
	"release: an operator runs the command (most need a reboot, an outage) and pg_sage " +
	"verifies the running value afterwards"

var (
	groupNamePattern    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9.-]{0,254}$`)
	instanceNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,97}$`)
	projectPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	regionPattern       = regexp.MustCompile(`^[a-z]{2}(-gov)?-[a-z]+-[0-9]$`)
)

// Target is the provider resource a change applies to, as resolved
// through the provider's API (Known) or not (Unresolved says why).
type Target struct {
	Provider                string                `json:"provider"`
	Region                  string                `json:"region,omitempty"`
	Project                 string                `json:"project,omitempty"`
	InstanceID              string                `json:"instance_id,omitempty"`
	ParameterGroup          string                `json:"parameter_group,omitempty"`
	ParameterGroupIsDefault bool                  `json:"parameter_group_is_default"`
	ParameterGroupStatus    string                `json:"parameter_group_status,omitempty"`
	Params                  map[string]GroupParam `json:"-"`
	Flags                   map[string]string     `json:"-"`
	FlagsKnown              bool                  `json:"flags_known"`
	Known                   bool                  `json:"known"`
	Unresolved              string                `json:"unresolved,omitempty"`
}

// GroupParam is one parameter of an RDS/Aurora parameter group.
type GroupParam struct {
	Value      string `json:"value"`
	Source     string `json:"source"`     // user | engine-default | system
	ApplyType  string `json:"apply_type"` // static | dynamic
	Modifiable bool   `json:"modifiable"`
}

// Rollback undoes a managed change.
type Rollback struct {
	Value       string `json:"value,omitempty"`
	Reset       bool   `json:"reset"`
	ApplyMethod string `json:"apply_method,omitempty"`
	CLI         string `json:"cli"`
}

// Proposal is one typed managed change.
type Proposal struct {
	Provider          string   `json:"provider"`
	Mechanism         string   `json:"mechanism"`
	Target            string   `json:"target"`
	Region            string   `json:"region,omitempty"`
	Project           string   `json:"project,omitempty"`
	Instance          string   `json:"instance,omitempty"`
	ParameterGroup    string   `json:"parameter_group,omitempty"`
	Parameter         string   `json:"parameter"`
	Value             string   `json:"value"`
	PGValue           string   `json:"pg_value"`
	Unit              string   `json:"unit,omitempty"`
	CurrentValue      string   `json:"current_value,omitempty"`
	RunningValue      string   `json:"running_value,omitempty"`
	ApplyMethod       string   `json:"apply_method"`
	RebootRequired    bool     `json:"reboot_required"`
	Rollback          Rollback `json:"rollback"`
	CLI               string   `json:"cli"`
	ConsoleURL        string   `json:"console_url"`
	Notes             []string `json:"notes,omitempty"`
	Blockers          []string `json:"blockers,omitempty"`
	RequiresApproval  bool     `json:"requires_approval"`
	AutoApply         bool     `json:"auto_apply"`
	AutoApplyWithheld string   `json:"auto_apply_withheld"`
	Reason            string   `json:"reason,omitempty"`
	Fingerprint       string   `json:"fingerprint"`
}

// Build turns an intent into a proposal for target. running is the
// setting as PostgreSQL runs it (nil when unknown).
func Build(intent Intent, target Target, running *collector.PGSetting) (Proposal, error) {
	if err := intent.validate(); err != nil {
		return Proposal{}, err
	}
	provider := NormalizeProvider(intent.Provider)
	if NormalizeProvider(target.Provider) != provider {
		return Proposal{}, fmt.Errorf("%w: intent for %s, target on %q", ErrUnsupportedProvider,
			provider, target.Provider)
	}
	if err := validateTarget(target); err != nil {
		return Proposal{}, err
	}
	value, unit, err := ProviderValue(provider, intent.Parameter, intent.Value)
	if err != nil {
		return Proposal{}, err
	}
	p := Proposal{Provider: provider, Parameter: intent.Parameter, Value: value,
		PGValue: intent.Value, Unit: unit, Reason: intent.Reason, RequiresApproval: true,
		AutoApply: false, AutoApplyWithheld: AutoApplyWithheld}
	if running != nil {
		p.RunningValue = running.Setting
	}
	if provider == "cloud-sql" {
		err = buildCloudSQL(&p, target)
	} else {
		err = buildRDS(&p, target)
	}
	if err != nil {
		return Proposal{}, err
	}
	p.Fingerprint = fingerprint(p)
	return p, nil
}

func validateTarget(t Target) error {
	bad := func(what, v string) error { return fmt.Errorf("%w: %s %q", ErrInvalidTarget, what, v) }
	switch {
	case t.ParameterGroup != "" && !groupNamePattern.MatchString(t.ParameterGroup):
		return bad("parameter group", t.ParameterGroup)
	case t.InstanceID != "" && !instanceNamePattern.MatchString(t.InstanceID):
		return bad("instance", t.InstanceID)
	case t.Project != "" && !projectPattern.MatchString(t.Project):
		return bad("project", t.Project)
	case t.Region != "" && !regionPattern.MatchString(t.Region):
		return bad("region", t.Region)
	}
	return nil
}

// rebootFor decides the apply method: the group's ApplyType when known,
// else PostgreSQL's restart list.
func rebootFor(parameter string, gp GroupParam, known bool) bool {
	if known && gp.ApplyType != "" {
		return gp.ApplyType == "static"
	}
	return pgconf.RequiresRestart(parameter)
}

func fingerprint(p Proposal) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{p.Provider, p.Target, p.Parameter,
		p.Value}, "|")))
	return hex.EncodeToString(sum[:])[:16]
}
