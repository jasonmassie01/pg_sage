package decommission

import (
	"net/url"
	"regexp"
	"strings"
)

// deleteTemplate is the operator's per-provider delete runbook for one
// item: comment lines start with "#" (shell) or "--" (SQL). Every value is
// quoted for the shell or as an SQL identifier, and no line carries a
// control character, so a template is safe to paste after review.
func deleteTemplate(it Item) []string {
	switch it.Kind {
	case KindProviderResource:
		return providerTemplate(it)
	case KindRDSFinalSnapshot:
		return rdsSnapshotTemplate(it)
	case KindLocalSchema:
		return []string{
			"-- on the control database " + sqlIdent(it.Account) +
				": the schema pg_sage created for deployment " + clean(it.DeploymentID),
			"DROP SCHEMA " + sqlIdent(it.ResourceID) + " CASCADE;",
		}
	case KindLocalDatabase:
		return []string{
			"-- on the server of the control database " + sqlIdent(it.Account) +
				": the database pg_sage created for deployment " + clean(it.DeploymentID),
			"DROP DATABASE " + sqlIdent(it.ResourceID) + ";",
		}
	case KindAgentSageSchema:
		return sageSchemaTemplate(it)
	}
	return []string{"# no delete template for item kind " + shellQuote(it.Kind)}
}

func providerTemplate(it Item) []string {
	id := it.ResourceID
	var lines []string
	if id == "" {
		id = it.DeterministicName
		lines = append(lines, "# no resource id was recorded (uncertain create): find the "+
			"resource named "+shellQuote(id)+" and confirm its pg_sage tags before deleting")
	}
	q, region, account := shellQuote(id), placeholder(it.Region, "<region>"),
		placeholder(it.Account, "<project>")
	switch it.Provider {
	case providerRDS:
		return append(lines,
			"# RDS instance "+q+": choose its final snapshot. Keep the flag below with a "+
				"name you pick, or replace it with --skip-final-snapshot",
			"aws rds delete-db-instance --region "+region+" --db-instance-identifier "+q+
				" --final-db-snapshot-identifier <choose-a-final-snapshot-name>")
	case providerCloudSQL:
		return append(lines,
			"gcloud sql instances patch "+q+" --project="+account+" --no-deletion-protection",
			"gcloud sql instances delete "+q+" --project="+account)
	case providerNeon:
		if it.Mode == "project" {
			return append(lines, "neonctl projects delete "+q)
		}
		return append(lines, "neonctl branches delete "+q+" --project-id "+account)
	case providerSupabase:
		if it.Mode == "project" {
			return append(lines, "supabase projects delete "+q)
		}
		return append(lines, "supabase branches delete "+q+" --project-ref "+account)
	case providerLakebase:
		path := "/api/2.0/postgres/projects/" + url.PathEscape(clean(it.Account)) +
			"/branches/" + url.PathEscape(clean(id))
		return append(lines, "databricks api delete "+shellQuote(path))
	}
	return append(lines, "# no delete template for provider "+shellQuote(it.Provider)+
		": delete resource "+q+" with that provider's own tools")
}

// rdsIdentifier is what RDS accepts as an identifier; anything else is listed
// by hand rather than spliced into a JMESPath query.
var rdsIdentifier = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*$`)

func rdsSnapshotTemplate(it Item) []string {
	region := placeholder(it.Region, "<region>")
	prefix := it.DeterministicName
	lines := []string{"# final snapshots are named " + shellQuote(prefix) +
		" plus the UTC destroy time (yyyymmddhhmmss); list them, then delete each one " +
		"you do not keep"}
	if rdsIdentifier.MatchString(prefix) {
		lines = append(lines, "aws rds describe-db-snapshots --region "+region+
			" --snapshot-type manual --query \"DBSnapshots[?starts_with("+
			"DBSnapshotIdentifier, '"+prefix+"')].DBSnapshotIdentifier\"")
	} else {
		lines = append(lines, "aws rds describe-db-snapshots --region "+region+
			" --snapshot-type manual")
	}
	return append(lines, "aws rds delete-db-snapshot --region "+region+
		" --db-snapshot-identifier <snapshot-id>")
}

func sageSchemaTemplate(it Item) []string {
	where := "-- connect to " + clean(it.ResourceID)
	if it.EnvRef != "" {
		where += " (DSN in " + clean(it.EnvRef) + ")"
	}
	return []string{
		where + ": pg_sage's own schema, left by the fleet sync of agent databases",
		"DROP SCHEMA sage CASCADE;",
	}
}

// placeholder quotes value, or returns the angle-bracket placeholder the
// operator must fill in when the value was never recorded.
func placeholder(value, fill string) string {
	if value == "" || value == unknown {
		return fill
	}
	return shellQuote(value)
}

// shellQuote single-quotes a value for a POSIX shell.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(clean(value), "'", `'\''`) + "'"
}

// sqlIdent double-quotes an SQL identifier.
func sqlIdent(value string) string {
	return `"` + strings.ReplaceAll(clean(value), `"`, `""`) + `"`
}

// clean replaces control characters, so a value can never start a new line
// of a template.
func clean(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, value)
}

// templateLine renders one template for a log line.
func templateLine(lines []string) string {
	return strings.Join(lines, " ; ")
}
