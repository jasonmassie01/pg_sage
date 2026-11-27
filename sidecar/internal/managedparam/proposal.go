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
	// MechanismServerParameter is an Azure flexible-server parameter.
	MechanismServerParameter = "server_parameter"
	// MechanismClusterSpec is a CloudNativePG Cluster's
	// spec.postgresql.parameters entry (market-12).
	MechanismClusterSpec = "cluster_spec"
	ApplyImmediate       = "immediate"
	ApplyPendingReboot   = "pending-reboot"
	// ApplyRollingRestart: the CNPG operator restarts the instances itself.
	ApplyRollingRestart = "rolling-restart"
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
	// gcpRegionPattern is a Google Cloud region (us-central1, europe-west4).
	gcpRegionPattern = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]{1,2}$`)
	// Azure names: a flexible server is 3-63 lower-case letters, digits
	// and hyphens; a resource group may also hold . _ ( ) (it is always
	// single-quoted), not ending in a period; a subscription is a GUID
	// (other ARM-safe identifiers are accepted, unquoted only when safe).
	azureServerPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	resourceGroupPattern = regexp.MustCompile(`^[A-Za-z0-9._()-]{0,89}[A-Za-z0-9_()-]$`)
	subscriptionPattern  = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

// InstanceRole values: an Aurora instance's role in its cluster, when
// known ("" for an instance outside any cluster).
const (
	InstanceRoleWriter  = "writer"
	InstanceRoleReader  = "reader"
	InstanceRoleUnknown = "unknown"
)

// Target is the provider resource a change applies to, as resolved
// through the provider's API (Known) or not (Unresolved says why).
type Target struct {
	Provider                string `json:"provider"`
	Region                  string `json:"region,omitempty"`
	Project                 string `json:"project,omitempty"`
	Subscription            string `json:"subscription,omitempty"`   // Azure
	ResourceGroup           string `json:"resource_group,omitempty"` // Azure
	InstanceID              string `json:"instance_id,omitempty"`
	ParameterGroup          string `json:"parameter_group,omitempty"`
	ParameterGroupIsDefault bool   `json:"parameter_group_is_default"`
	ParameterGroupStatus    string `json:"parameter_group_status,omitempty"`
	// Cluster is the Aurora, AlloyDB or CNPG cluster; Namespace and
	// UpdateStrategy (spec.primaryUpdateStrategy) are CNPG's.
	Cluster        string                `json:"cluster,omitempty"`
	Namespace      string                `json:"namespace,omitempty"`
	UpdateStrategy string                `json:"update_strategy,omitempty"`
	InstanceRole   string                `json:"instance_role,omitempty"`
	Params         map[string]GroupParam `json:"-"`
	Flags          map[string]string     `json:"-"`
	FlagsKnown     bool                  `json:"flags_known"`
	Known          bool                  `json:"known"`
	Unresolved     string                `json:"unresolved,omitempty"`
}

// GroupParam is one parameter of an RDS/Aurora parameter group or an
// Azure server (Default and Unit are Azure's).
type GroupParam struct {
	Value      string `json:"value"`
	Default    string `json:"default,omitempty"`
	Source     string `json:"source"` // user | engine-default | system
	Unit       string `json:"unit,omitempty"`
	ApplyType  string `json:"apply_type"` // static | dynamic
	Modifiable bool   `json:"modifiable"`
}

// Rollback undoes a managed change.
type Rollback struct {
	Value       string `json:"value,omitempty"`
	Reset       bool   `json:"reset"`
	ApplyMethod string `json:"apply_method,omitempty"`
	CLI         string `json:"cli"`
	// Patch is the CNPG merge patch that restores the prior value.
	Patch string `json:"patch,omitempty"`
}

// Conflict is a different value a peer database of the same provider
// resource proposes for the parameter.
type Conflict struct {
	Database string `json:"database"`
	Value    string `json:"value"`
}

// Proposal is one typed managed change.
type Proposal struct {
	Provider          string     `json:"provider"`
	Mechanism         string     `json:"mechanism"`
	Target            string     `json:"target"`
	Region            string     `json:"region,omitempty"`
	Project           string     `json:"project,omitempty"`
	ResourceGroup     string     `json:"resource_group,omitempty"`
	Cluster           string     `json:"cluster,omitempty"`
	Namespace         string     `json:"namespace,omitempty"`
	Instance          string     `json:"instance,omitempty"`
	ParameterGroup    string     `json:"parameter_group,omitempty"`
	Parameter         string     `json:"parameter"`
	Value             string     `json:"value"`
	PGValue           string     `json:"pg_value"`
	Unit              string     `json:"unit,omitempty"`
	CurrentValue      string     `json:"current_value,omitempty"`
	RunningValue      string     `json:"running_value,omitempty"`
	ApplyMethod       string     `json:"apply_method"`
	RebootRequired    bool       `json:"reboot_required"`
	Rollback          Rollback   `json:"rollback"`
	CLI               string     `json:"cli"`
	Patch             string     `json:"patch,omitempty"`  // CNPG merge patch
	GitOps            string     `json:"gitops,omitempty"` // CNPG manifest fragment
	ConsoleURL        string     `json:"console_url"`
	Notes             []string   `json:"notes,omitempty"`
	Blockers          []string   `json:"blockers,omitempty"`
	Conflicts         []Conflict `json:"conflicts,omitempty"`
	RequiresApproval  bool       `json:"requires_approval"`
	AutoApply         bool       `json:"auto_apply"`
	AutoApplyWithheld string     `json:"auto_apply_withheld"`
	Reason            string     `json:"reason,omitempty"`
	Fingerprint       string     `json:"fingerprint"`
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
	if err := validateTarget(provider, target); err != nil {
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
	switch provider {
	case "cloud-sql":
		err = buildCloudSQL(&p, target)
	case "alloydb":
		err = buildAlloyDB(&p, target)
	case "azure":
		err = buildAzure(&p, target)
	case "cnpg":
		err = buildCNPG(&p, target)
	default:
		err = buildRDS(&p, target)
	}
	if err != nil {
		return Proposal{}, err
	}
	p.Fingerprint = fingerprint(p)
	return p, nil
}

// validateTarget checks every name a command may carry, with the
// provider's own instance and region forms.
func validateTarget(provider string, t Target) error {
	bad := func(what, v string) error { return fmt.Errorf("%w: %s %q", ErrInvalidTarget, what, v) }
	instance, region := instanceNamePattern, regionPattern
	switch provider {
	case "azure":
		instance = azureServerPattern
	case "alloydb":
		region = gcpRegionPattern
	case "cnpg":
		return validateCNPGTarget(t)
	}
	switch {
	case t.ParameterGroup != "" && !groupNamePattern.MatchString(t.ParameterGroup):
		return bad("parameter group", t.ParameterGroup)
	case t.InstanceID != "" && !instance.MatchString(t.InstanceID):
		return bad("instance", t.InstanceID)
	case t.Project != "" && !projectPattern.MatchString(t.Project):
		return bad("project", t.Project)
	case t.Region != "" && !region.MatchString(t.Region):
		return bad("region", t.Region)
	case provider == "alloydb" && t.Cluster != "" && !instanceNamePattern.MatchString(t.Cluster):
		return bad("cluster", t.Cluster)
	case t.ResourceGroup != "" && !resourceGroupPattern.MatchString(t.ResourceGroup):
		return bad("resource group", t.ResourceGroup)
	case t.Subscription != "" && !subscriptionPattern.MatchString(t.Subscription):
		return bad("subscription", t.Subscription)
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

// fingerprintVersion prefixes the fingerprint identity: the provider,
// parameter and value of the change. The target is not part of it: the
// proposals table lives in the monitored database's own sage schema, so
// the database is implicit, and the provider resource a transient
// resolution failure replaces with placeholders is a refreshable
// attribute. The schema migration ddlManagedChangeFingerprintV2 computes
// the same value in SQL.
const fingerprintVersion = "v2:"

func fingerprint(p Proposal) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{p.Provider, p.Parameter, p.Value},
		"|")))
	return fingerprintVersion + hex.EncodeToString(sum[:])[:16]
}

// placeholderTarget reports that p was built without a resolved provider
// resource: its target is empty or carries a <placeholder>, which no
// validated group, project or instance name can contain.
func (p Proposal) placeholderTarget() bool {
	return p.Target == "" || strings.Contains(p.Target, "<")
}
