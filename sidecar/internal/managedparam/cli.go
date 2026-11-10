package managedparam

import (
	"fmt"
	"net/url"
)

// Command rules: parameter names, the intent's value and the target's
// group, region, project and instance names are validated before any
// command is built (intentValuePattern, validateTarget). Values read back
// from the provider (current flags, current group values) are checked
// against providerValuePattern and always single-quoted; one that fails
// gives a blocker and a comment line instead of a runnable command.

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
	var blocker string
	p.Rollback, blocker = rdsRollback(region, group, p.Parameter, p.ApplyMethod, gp, inGroup)
	if blocker != "" {
		p.Blockers = append(p.Blockers, blocker)
	}
	p.ConsoleURL = rdsConsoleURL(t)
	rdsNotes(p, t)
	return nil
}

func rdsModify(region, group, parameter, value, method string) string {
	return fmt.Sprintf("aws rds modify-db-parameter-group --region %s "+
		"--db-parameter-group-name %s --parameters %s", region, group,
		rdsParametersArg(parameter, value, method))
}

// rdsRollback restores a user-set value, else resets to the engine
// default. A user-set value that cannot be quoted safely gives no command
// and a blocker.
func rdsRollback(region, group, parameter, method string, gp GroupParam,
	inGroup bool) (Rollback, string) {
	if inGroup && gp.Source == "user" && gp.Value != "" {
		if err := checkProviderValue(parameter, gp.Value); err != nil {
			blocker := err.Error() + ": note the current value in the console before " +
				"this change and restore it there if it has to be undone"
			return Rollback{ApplyMethod: method,
				CLI: "# no rollback command: see the blockers"}, blocker
		}
		return Rollback{Value: gp.Value, ApplyMethod: method,
			CLI: rdsModify(region, group, parameter, gp.Value, method)}, ""
	}
	return Rollback{Reset: true, ApplyMethod: method, CLI: fmt.Sprintf(
		"aws rds reset-db-parameter-group --region %s --db-parameter-group-name %s "+
			"--parameters %s", region, group, rdsParametersArg(parameter, "", method))}, ""
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
		auroraMemberNotes(p, t)
	}
	if p.RebootRequired {
		p.Notes = append(p.Notes, "The change is pending until the instance reboots; the "+
			"reboot is an outage (Multi-AZ shortens it with a failover).")
	}
}

// auroraMemberNotes names the cluster of an Aurora instance and, for a
// reader, warns that its parameter group may not be the writer's.
func auroraMemberNotes(p *Proposal, t Target) {
	if t.Cluster == "" {
		return
	}
	instance := orPlaceholder(t.InstanceID, placeholderID)
	switch t.InstanceRole {
	case InstanceRoleWriter:
		p.Notes = append(p.Notes, fmt.Sprintf("%s is the writer of Aurora cluster %s.",
			instance, t.Cluster))
	case InstanceRoleReader:
		p.Notes = append(p.Notes, fmt.Sprintf("%s is a reader of Aurora cluster %s: its DB "+
			"parameter group may differ from the writer's, so this change reaches only the "+
			"instances that use %s; change the writer's group too if the writer should "+
			"run it.", instance, t.Cluster, orPlaceholder(t.ParameterGroup, placeholderGroup)))
	default:
		p.Notes = append(p.Notes, fmt.Sprintf("pg_sage could not tell whether %s is the "+
			"writer or a reader of Aurora cluster %s (rds:DescribeDBClusters failed): check "+
			"that %s is the group the intended instances use.", instance, t.Cluster,
			orPlaceholder(t.ParameterGroup, placeholderGroup)))
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
	p.Rollback = Rollback{Reset: !hadPrior, ApplyMethod: p.ApplyMethod}
	if hadPrior {
		p.Rollback.Value = prior
	}
	cloudSQLCommands(p, t, instance, project)
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

// cloudSQLCommands fills the forward and rollback commands. Without the
// current flags neither can be built (--database-flags replaces the whole
// list), so both are fill-in templates; a flag that cannot be quoted
// safely withholds both.
func cloudSQLCommands(p *Proposal, t Target, instance, project string) {
	if !t.FlagsKnown {
		describe := fmt.Sprintf("gcloud sql instances describe %s --project=%s "+
			"--format='json(settings.databaseFlags)'", instance, project)
		p.CLI = fmt.Sprintf("# fill in: %s, then run gcloud sql instances patch %s "+
			"--project=%s with every current flag plus %s=%s", describe, instance, project,
			p.Parameter, p.Value)
		p.Rollback.CLI = "# fill in: keep the flag list from " + describe + " before the " +
			"change; patching the instance with that list restores it"
		return
	}
	next := copyFlags(t.Flags)
	next[p.Parameter] = p.Value
	cli, err := gcloudFlags(instance, project, next)
	rollback, rerr := gcloudFlags(instance, project, copyFlags(t.Flags))
	if rerr != nil {
		// The rollback list holds every current flag, so its error names
		// the first unsafe one even when the change replaces it.
		err = rerr
	}
	if err != nil {
		p.CLI = "# no command: a current flag cannot be quoted safely (see the blockers)"
		p.Rollback.CLI = "# no rollback command: see the blockers"
		if checkProviderValue(p.Parameter, p.Rollback.Value) != nil {
			p.Rollback.Value = ""
		}
		p.Blockers = append(p.Blockers, err.Error()+": fix or remove that flag in the "+
			"console, or make this change there")
		return
	}
	p.CLI, p.Rollback.CLI = cli, rollback
}

// gcloudFlags builds the patch command for the complete flag list. Only a
// list known to be empty clears the flags.
func gcloudFlags(instance, project string, flags map[string]string) (string, error) {
	cmd := fmt.Sprintf("gcloud sql instances patch %s --project=%s", instance, project)
	if len(flags) == 0 {
		return cmd + " --clear-database-flags", nil
	}
	dict, err := gcloudDict(flags)
	if err != nil {
		return "", err
	}
	return cmd + " --database-flags=" + shellQuote(dict), nil
}

func cloudSQLNotes(p *Proposal, t Target) {
	if !t.FlagsKnown {
		p.Blockers = append(p.Blockers, "The instance's current database flags are unknown ("+
			orPlaceholder(t.Unresolved, "cloud telemetry unavailable")+"): --database-flags "+
			"replaces every flag, so pg_sage gives a fill-in template instead of a command. "+
			"Read the existing flags (gcloud sql instances describe) and include them all.")
	}
	if p.RebootRequired {
		p.Notes = append(p.Notes, "Cloud SQL restarts the instance to apply this flag: an "+
			"outage of about a minute.")
	}
	p.Notes = append(p.Notes, "Cloud SQL supports a subset of PostgreSQL settings as flags; "+
		"gcloud rejects an unsupported one without changing the instance.")
}
