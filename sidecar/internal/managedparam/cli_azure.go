package managedparam

import (
	"fmt"
	"net/url"
	"strings"
)

// Azure flexible-server parameters (advisors-03). pg_sage reads them over
// ARM (configurations/read) to fill the proposal; it never writes one for
// a proposal: only the WAL-slot custodian's max_slot_wal_keep_size goes
// through ARM. The operator runs the az command.

const (
	placeholderSubscription  = "<subscription>"
	placeholderResourceGroup = "<resource-group>"
	placeholderServer        = "<server>"
)

// buildAzure fills an Azure server-parameter change. A described
// parameter gives its unit, read-only flag, restart need and prior value.
func buildAzure(p *Proposal, t Target) error {
	gp, described := t.Params[p.Parameter]
	described = described && t.Known
	if described && !gp.Modifiable {
		return fmt.Errorf("%w: azure server parameter %s on %s is read-only", ErrNotModifiable,
			p.Parameter, t.InstanceID)
	}
	if described && isAzureMemoryUnit(gp.Unit) {
		v, err := AzureValue(p.PGValue, gp.Unit)
		if err != nil {
			return fmt.Errorf("%w: azure server parameter %s: %v", ErrInvalidValue,
				p.Parameter, err)
		}
		p.Value, p.Unit = v, gp.Unit
	}
	p.Mechanism = MechanismServerParameter
	p.Instance, p.ResourceGroup = t.InstanceID, t.ResourceGroup
	p.Target = orPlaceholder(t.ResourceGroup, placeholderResourceGroup) + "/" +
		orPlaceholder(t.InstanceID, placeholderServer)
	p.RebootRequired = rebootFor(p.Parameter, gp, described)
	p.ApplyMethod = ApplyImmediate
	if p.RebootRequired {
		p.ApplyMethod = ApplyPendingReboot
	}
	if described {
		p.CurrentValue = gp.Value
	}
	azureCommands(p, t, gp, described)
	p.ConsoleURL = azureConsoleURL(t)
	azureNotes(p, t, described)
	return nil
}

// azureScope names the subscription and resource group of every command;
// the resource group may hold parentheses, so it is always quoted.
func azureScope(t Target) string {
	return "--subscription " + orPlaceholder(t.Subscription, placeholderSubscription) +
		" --resource-group " + shellQuote(orPlaceholder(t.ResourceGroup,
		placeholderResourceGroup))
}

func azureParameterCmd(verb string, t Target, parameter string) string {
	return fmt.Sprintf("az postgres flexible-server parameter %s %s --server-name %s "+
		"--name %s", verb, azureScope(t), orPlaceholder(t.InstanceID, placeholderServer),
		parameter)
}

// azureCommands fills the forward and rollback commands. An unresolved
// server gives fill-in templates, never a runnable command.
func azureCommands(p *Proposal, t Target, gp GroupParam, described bool) {
	set := azureParameterCmd("set", t, p.Parameter)
	p.CLI = set + " --value " + p.Value
	if p.RebootRequired {
		p.CLI += "\n" + fmt.Sprintf("az postgres flexible-server restart %s --name %s",
			azureScope(t), orPlaceholder(t.InstanceID, placeholderServer))
	}
	if !t.Known {
		p.CLI = fillIn(p.CLI)
		p.Rollback = Rollback{ApplyMethod: p.ApplyMethod, CLI: "# fill in: read the " +
			"current value first (" + azureParameterCmd("show", t, p.Parameter) + ") and " +
			"set it back with az postgres flexible-server parameter set"}
		return
	}
	var blocker string
	p.Rollback, blocker = azureRollback(set, p.Parameter, p.ApplyMethod, gp, described)
	if blocker != "" {
		p.Blockers = append(p.Blockers, blocker)
	}
}

// azureRollback restores a user-set value, else returns the parameter to
// the captured system default (as the custodian's ARM reset does). A value
// that cannot be quoted safely, or a missing default, gives no command.
func azureRollback(set, parameter, method string, gp GroupParam,
	described bool) (Rollback, string) {
	none := Rollback{ApplyMethod: method, CLI: "# no rollback command: see the blockers"}
	if !described {
		none.CLI = "# no rollback command: pg_sage did not read the current value; note it " +
			"before the change"
		return none, ""
	}
	if gp.Source == "user" {
		if err := checkProviderValue(parameter, gp.Value); err != nil {
			return none, err.Error() + ": note the current value before this change and " +
				"restore it in the portal if it has to be undone"
		}
		return Rollback{Value: gp.Value, ApplyMethod: method,
			CLI: set + " --value " + shellQuote(gp.Value)}, ""
	}
	if gp.Default == "" {
		return none, "Azure reported no default value for " + parameter + ": note the " +
			"current value before this change and restore it in the portal"
	}
	if err := checkProviderValue(parameter, gp.Default); err != nil {
		return none, err.Error() + ": reset it to the default in the portal if this " +
			"change has to be undone"
	}
	return Rollback{Value: gp.Default, Reset: true, ApplyMethod: method,
		CLI: set + " --value " + shellQuote(gp.Default) + " --source system-default"}, ""
}

// runningForm is the value pg_settings shows once p runs. Azure may report
// a parameter in another unit than pg_settings.unit (max_wal_size in KB,
// not MB), so an Azure proposal compares its PostgreSQL base-unit value.
func runningForm(p Proposal) string {
	if p.Mechanism == MechanismClusterSpec {
		return cnpgRunningForm(p.Parameter, p.Value)
	}
	if p.Mechanism != MechanismServerParameter {
		return p.Value
	}
	if v, _, err := ProviderValue(p.Provider, p.Parameter, p.PGValue); err == nil {
		return v
	}
	return p.Value
}

// fillIn turns every line of a command into a comment to complete.
func fillIn(cli string) string {
	lines := strings.Split(cli, "\n")
	for i, l := range lines {
		lines[i] = "# fill in: " + l
	}
	return strings.Join(lines, "\n")
}

func azureConsoleURL(t Target) string {
	if !t.Known || t.Subscription == "" || t.ResourceGroup == "" || t.InstanceID == "" {
		return "https://portal.azure.com/#browse/Microsoft.DBforPostgreSQL%2FflexibleServers"
	}
	return fmt.Sprintf("https://portal.azure.com/#resource/subscriptions/%s/resourceGroups/"+
		"%s/providers/Microsoft.DBforPostgreSQL/flexibleServers/%s/serverParameters",
		url.PathEscape(t.Subscription), url.PathEscape(t.ResourceGroup),
		url.PathEscape(t.InstanceID))
}

func azureNotes(p *Proposal, t Target, described bool) {
	switch {
	case !t.Known:
		p.Blockers = append(p.Blockers, "The azure server is unknown ("+
			orPlaceholder(t.Unresolved, "cloud telemetry unavailable")+"): set the azure: "+
			"section (subscription_id, resource_group, and server_name when the host does "+
			"not name the server) or cloud_resource.azure, with an identity that holds "+
			"Microsoft.DBforPostgreSQL/flexibleServers/configurations/read, so pg_sage can "+
			"read the parameter's unit, read-only flag and restart need. The command is a "+
			"fill-in template.")
	case !described:
		p.Blockers = append(p.Blockers, fmt.Sprintf("Azure did not describe %s on server %s "+
			"(no such server parameter, or it was not read): check it with %s before "+
			"running this change", p.Parameter, t.InstanceID,
			azureParameterCmd("show", t, p.Parameter)))
	}
	if p.RebootRequired {
		p.Notes = append(p.Notes, "Azure applies "+p.Parameter+" only after the server "+
			"restarts (az postgres flexible-server restart): an outage, usually a minute "+
			"or two.")
	}
	p.Notes = append(p.Notes, "pg_sage reads server parameters over ARM but writes only "+
		"the WAL-slot custodian's max_slot_wal_keep_size; an operator runs this command.")
}
