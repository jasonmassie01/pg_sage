package managedparam

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// The commands are built only from validated names and values (no
// spaces, quotes or shell metacharacters can reach them).

const (
	placeholderGroup  = "<db-parameter-group>"
	placeholderRegion = "<region>"
	placeholderID     = "<db-instance-identifier>"
)

func orPlaceholder(v, placeholder string) string {
	if v == "" {
		return placeholder
	}
	return v
}

// buildRDS fills an RDS/Aurora DB parameter group change.
func buildRDS(p *Proposal, t Target) error {
	gp, inGroup := t.Params[p.Parameter]
	if t.Known && inGroup && !gp.Modifiable {
		return fmt.Errorf("%w: %s in %s", ErrNotModifiable, p.Parameter, t.ParameterGroup)
	}
	p.Mechanism = MechanismParameterGroup
	p.Region, p.Instance, p.ParameterGroup = t.Region, t.InstanceID, t.ParameterGroup
	group := orPlaceholder(t.ParameterGroup, placeholderGroup)
	region := orPlaceholder(t.Region, placeholderRegion)
	p.Target = group
	p.RebootRequired = rebootFor(p.Parameter, gp, t.Known && inGroup)
	p.ApplyMethod = ApplyImmediate
	if p.RebootRequired {
		p.ApplyMethod = ApplyPendingReboot
	}
	if inGroup {
		p.CurrentValue = gp.Value
	}
	p.CLI = rdsModify(region, group, p.Parameter, p.Value, p.ApplyMethod)
	if p.RebootRequired {
		p.CLI += "\n" + fmt.Sprintf("aws rds reboot-db-instance --region %s "+
			"--db-instance-identifier %s", region, orPlaceholder(t.InstanceID, placeholderID))
	}
	p.Rollback = rdsRollback(region, group, p.Parameter, p.ApplyMethod, gp, inGroup)
	p.ConsoleURL = rdsConsoleURL(t)
	rdsNotes(p, t)
	return nil
}

func rdsModify(region, group, parameter, value, method string) string {
	return fmt.Sprintf("aws rds modify-db-parameter-group --region %s "+
		"--db-parameter-group-name %s --parameters "+
		`"ParameterName=%s,ParameterValue=%s,ApplyMethod=%s"`, region, group, parameter,
		value, method)
}

// rdsRollback restores a user-set value, else resets to the engine default.
func rdsRollback(region, group, parameter, method string, gp GroupParam,
	inGroup bool) Rollback {
	if inGroup && gp.Source == "user" && gp.Value != "" {
		return Rollback{Value: gp.Value, ApplyMethod: method,
			CLI: rdsModify(region, group, parameter, gp.Value, method)}
	}
	return Rollback{Reset: true, ApplyMethod: method, CLI: fmt.Sprintf(
		"aws rds reset-db-parameter-group --region %s --db-parameter-group-name %s "+
			`--parameters "ParameterName=%s,ApplyMethod=%s"`, region, group, parameter, method)}
}

func rdsConsoleURL(t Target) string {
	region := orPlaceholder(t.Region, "us-east-1")
	base := fmt.Sprintf("https://%s.console.aws.amazon.com/rds/home?region=%s", region, region)
	if t.ParameterGroup == "" {
		return base + "#parameter-groups:"
	}
	return base + "#parameter-group-details:parameter-group-name=" +
		url.QueryEscape(t.ParameterGroup)
}

func rdsNotes(p *Proposal, t Target) {
	if !t.Known {
		why := t.Unresolved
		if why == "" {
			why = "cloud telemetry is not configured"
		}
		p.Notes = append(p.Notes, "The parameter group is unknown ("+why+"): replace the "+
			"placeholders with the instance's own DB parameter group and region. A default "+
			"group (default.*) cannot be modified.")
	}
	if t.ParameterGroupIsDefault {
		p.Blockers = append(p.Blockers, fmt.Sprintf("%s uses the default parameter group "+
			"%s, which cannot be modified: create a custom group (aws rds "+
			"create-db-parameter-group --db-parameter-group-family <family> "+
			"--db-parameter-group-name <name> --description pg_sage), attach it with aws rds "+
			"modify-db-instance --db-parameter-group-name <name> (a reboot applies it), then "+
			"run this change against the new group", orPlaceholder(t.InstanceID, "The instance"),
			t.ParameterGroup))
	}
	if p.Provider == "aurora" {
		p.Notes = append(p.Notes, "Aurora: this is an instance-level parameter in the "+
			"writer's DB parameter group; cluster-level parameters live in the DB cluster "+
			"parameter group (modify-db-cluster-parameter-group). Apply it to every "+
			"instance that should run it.")
	}
	if p.RebootRequired {
		p.Notes = append(p.Notes, "The change is pending until the instance reboots; the "+
			"reboot is an outage (Multi-AZ shortens it with a failover).")
	}
}

// buildCloudSQL fills a Cloud SQL database flag change. --database-flags
// replaces the instance's whole flag list, so the command carries every
// current flag with the change applied.
func buildCloudSQL(p *Proposal, t Target) error {
	p.Mechanism = MechanismDatabaseFlag
	p.Project, p.Instance = t.Project, t.InstanceID
	project := orPlaceholder(t.Project, "<project>")
	instance := orPlaceholder(t.InstanceID, "<instance>")
	p.Target = project + ":" + instance
	p.RebootRequired = rebootFor(p.Parameter, GroupParam{}, false)
	p.ApplyMethod = ApplyImmediate
	if p.RebootRequired {
		p.ApplyMethod = ApplyPendingReboot
	}
	prior, hadPrior := t.Flags[p.Parameter]
	if hadPrior {
		p.CurrentValue = prior
	}
	next := copyFlags(t.Flags)
	next[p.Parameter] = p.Value
	p.CLI = gcloudFlags(instance, project, next)
	rollback := copyFlags(t.Flags)
	p.Rollback = Rollback{Reset: !hadPrior, ApplyMethod: p.ApplyMethod}
	if hadPrior {
		p.Rollback.Value = prior
	}
	p.Rollback.CLI = gcloudFlags(instance, project, rollback)
	p.ConsoleURL = fmt.Sprintf("https://console.cloud.google.com/sql/instances/%s/edit?"+
		"project=%s", url.PathEscape(instance), url.QueryEscape(project))
	cloudSQLNotes(p, t)
	return nil
}

func copyFlags(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func gcloudFlags(instance, project string, flags map[string]string) string {
	cmd := fmt.Sprintf("gcloud sql instances patch %s --project=%s", instance, project)
	if len(flags) == 0 {
		return cmd + " --clear-database-flags"
	}
	pairs := make([]string, 0, len(flags))
	for k, v := range flags {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return cmd + " --database-flags=" + strings.Join(pairs, ",")
}

func cloudSQLNotes(p *Proposal, t Target) {
	if !t.FlagsKnown {
		p.Blockers = append(p.Blockers, "The instance's current database flags are unknown ("+
			orPlaceholder(t.Unresolved, "cloud telemetry unavailable")+"): --database-flags "+
			"replaces every flag, so this command would clear the others. Add the existing "+
			"flags (gcloud sql instances describe) before running it.")
	}
	if p.RebootRequired {
		p.Notes = append(p.Notes, "Cloud SQL restarts the instance to apply this flag: an "+
			"outage of about a minute.")
	}
	p.Notes = append(p.Notes, "Cloud SQL supports a subset of PostgreSQL settings as flags; "+
		"gcloud rejects an unsupported one without changing the instance.")
}
