package managedparam

import (
	"fmt"
	"net/url"
)

// AlloyDB database flags (advisors-03). --database-flags replaces the
// instance's whole flag list, so, as on Cloud SQL, a command is built only
// from a known flag list. Until clouds-09 reads AlloyDB flags no resolver
// knows them, and every proposal is a fill-in template with a blocker.

// alloyIDs are the names an AlloyDB command carries, or placeholders.
type alloyIDs struct{ project, region, cluster, instance string }

func alloyIDsOf(t Target) alloyIDs {
	return alloyIDs{project: orPlaceholder(t.Project, "<project>"),
		region:   orPlaceholder(t.Region, placeholderRegion),
		cluster:  orPlaceholder(t.Cluster, "<cluster>"),
		instance: orPlaceholder(t.InstanceID, "<instance>")}
}

func (id alloyIDs) command(verb string) string {
	return fmt.Sprintf("gcloud alloydb instances %s %s --cluster=%s --region=%s --project=%s",
		verb, id.instance, id.cluster, id.region, id.project)
}

// buildAlloyDB fills an AlloyDB database flag change.
func buildAlloyDB(p *Proposal, t Target) error {
	p.Mechanism = MechanismDatabaseFlag
	p.Project, p.Region, p.Cluster, p.Instance = t.Project, t.Region, t.Cluster, t.InstanceID
	id := alloyIDsOf(t)
	p.Target = fmt.Sprintf("projects/%s/locations/%s/clusters/%s/instances/%s", id.project,
		id.region, id.cluster, id.instance)
	p.RebootRequired = rebootFor(p.Parameter, GroupParam{}, false)
	p.ApplyMethod = ApplyImmediate
	if p.RebootRequired {
		p.ApplyMethod = ApplyPendingReboot
	}
	prior, hadPrior := t.Flags[p.Parameter]
	hadPrior = hadPrior && t.FlagsKnown
	if hadPrior {
		p.CurrentValue = prior
	}
	p.Rollback = Rollback{Reset: !hadPrior, ApplyMethod: p.ApplyMethod}
	if hadPrior {
		p.Rollback.Value = prior
	}
	alloyDBCommands(p, t, id)
	p.ConsoleURL = "https://console.cloud.google.com/alloydb/clusters?project=" +
		url.QueryEscape(id.project)
	alloyDBNotes(p, t)
	return nil
}

// alloyDBCommands fills the forward and rollback commands from the known
// flag list (clouds-01's validated, quoted dictionary), else templates.
func alloyDBCommands(p *Proposal, t Target, id alloyIDs) {
	update := id.command("update")
	if !t.FlagsKnown {
		describe := id.command("describe") + " --format='json(databaseFlags)'"
		p.CLI = fmt.Sprintf("# fill in: %s, then run %s with every current flag plus %s=%s",
			describe, update, p.Parameter, p.Value)
		p.Rollback.CLI = "# fill in: keep the flag list from " + describe + " before the " +
			"change; updating the instance with that list restores it"
		return
	}
	next := copyFlags(t.Flags)
	next[p.Parameter] = p.Value
	dict, err := gcloudDict(next)
	prior, rerr := gcloudDict(copyFlags(t.Flags))
	if rerr != nil {
		err = rerr // the current list names the first unsafe flag
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
	p.CLI = update + " --database-flags=" + shellQuote(dict)
	if len(t.Flags) == 0 {
		p.Rollback.CLI = "# no rollback command: the instance had no database flags; " +
			"remove " + p.Parameter + " in the console (instance, Edit, Flags)"
		return
	}
	p.Rollback.CLI = update + " --database-flags=" + shellQuote(prior)
}

func alloyDBNotes(p *Proposal, t Target) {
	if !t.FlagsKnown {
		p.Blockers = append(p.Blockers, "The instance's current database flags are unknown ("+
			orPlaceholder(t.Unresolved, "pg_sage does not read AlloyDB flags yet")+"): "+
			"--database-flags replaces every flag, so pg_sage gives a fill-in template "+
			"instead of a command. Read the existing flags (gcloud alloydb instances "+
			"describe) and include them all.")
	}
	if !t.Known {
		p.Notes = append(p.Notes, "The AlloyDB instance is unknown: replace <project>, "+
			"<region>, <cluster> and <instance> with the primary instance's names.")
	}
	if p.RebootRequired {
		p.Notes = append(p.Notes, "AlloyDB restarts the instance to apply this flag: an "+
			"outage.")
	}
	p.Notes = append(p.Notes, "AlloyDB supports a subset of PostgreSQL settings as flags; "+
		"gcloud rejects an unsupported one without changing the instance.")
}
