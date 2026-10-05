package firstlook

import (
	"fmt"
	"strings"
)

// Capability names.
const (
	CapStatStatements = "pg_stat_statements"
	CapHypoPG         = "hypopg"
	CapAutoExplain    = "auto_explain"
)

// Capability statuses.
const (
	CapabilityOK          = "ok"
	CapabilityMissing     = "missing"     // available, not created
	CapabilityNotLoaded   = "not_loaded"  // needs a preload library
	CapabilityUnavailable = "unavailable" // not installed on the server
)

// Capability is one extension pg_sage uses, its state and the exact steps
// that enable it on this provider.
type Capability struct {
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Detail string   `json:"detail,omitempty"`
	Steps  []string `json:"steps,omitempty"`
}

// Extensions is what the catalog says about the extensions pg_sage uses.
// Nil pointers are unknown (the role may not read the setting).
type Extensions struct {
	Role                    string
	StatStatementsInstalled bool
	StatStatementsLoaded    *bool
	QueryTextVisible        *bool
	HypoPGInstalled         bool
	HypoPGAvailable         bool
	AutoExplainLoaded       bool
	PreloadLibraries        *string
}

// ExtensionCapabilities reports each extension with its enablement steps
// for provider, and an item for each one that is not ready (a warning for
// pg_stat_statements, which query analysis needs; info for the optional
// HypoPG and auto_explain), plus one when query text is hidden.
func ExtensionCapabilities(e Extensions, provider string) ([]Capability, []Item) {
	caps := []Capability{statStatements(e, provider), hypoPG(e, provider),
		autoExplain(e, provider)}
	var items []Item
	for _, c := range caps {
		if c.Status == CapabilityOK {
			continue
		}
		sev := SeverityInfo
		if c.Name == CapStatStatements {
			sev = SeverityWarning
		}
		items = append(items, Item{Rule: RuleMissingExtension, Severity: sev, Object: c.Name,
			Title:  fmt.Sprintf("%s is %s", c.Name, strings.ReplaceAll(c.Status, "_", " ")),
			Detail: c.Detail, Recommendation: strings.Join(c.Steps, " "),
			Evidence: []Evidence{{Source: "pg_extension",
				Ref:    "pg_extension, pg_available_extensions, shared_preload_libraries",
				Detail: fmt.Sprintf("%s status %s", c.Name, c.Status)}}})
	}
	if e.QueryTextVisible != nil && !*e.QueryTextVisible {
		items = append(items, queryTextItem(e.Role))
	}
	return caps, items
}

func queryTextItem(role string) Item {
	if role == "" {
		role = "sage_agent"
	}
	return Item{Rule: RuleQueryTextHidden, Severity: SeverityWarning,
		Object: CapStatStatements, Title: "Query text is hidden from pg_sage",
		Detail: "pg_stat_statements shows <insufficient privilege> for other roles' " +
			"queries, so query findings cannot name the statement.",
		Recommendation: "Grant pg_read_all_stats (pg_monitor includes it).",
		SuggestedSQL:   "GRANT pg_read_all_stats TO " + ident(role) + ";",
		Evidence: []Evidence{{Source: "pg_stat_statements",
			Ref: "pg_stat_statements.query = '<insufficient privilege>'"}}}
}

const createStatements = "CREATE EXTENSION IF NOT EXISTS pg_stat_statements;"

func statStatements(e Extensions, provider string) Capability {
	c := Capability{Name: CapStatStatements, Status: CapabilityOK,
		Detail: "Query statistics are collected."}
	loadedKnownFalse := e.StatStatementsLoaded != nil && !*e.StatStatementsLoaded
	loadedKnownTrue := e.StatStatementsLoaded != nil && *e.StatStatementsLoaded
	switch {
	case e.StatStatementsInstalled && !loadedKnownFalse:
		return c
	case loadedKnownTrue:
		c.Status, c.Detail = CapabilityMissing, "The library is loaded; the extension "+
			"is not created in this database."
		c.Steps = []string{createStatements}
		return c
	case loadedKnownFalse:
		c.Status = CapabilityNotLoaded
	default:
		c.Status = CapabilityMissing
	}
	c.Detail = "Without it pg_sage sees no query statistics: the first look and the " +
		"catalog rules still run, query tuning does not."
	c.Steps = preloadSteps(provider, "pg_stat_statements", e.PreloadLibraries)
	return c
}

// preloadSteps are the steps that add lib to shared_preload_libraries and
// create the extension, per provider.
func preloadSteps(provider, lib string, preload *string) []string {
	create := "CREATE EXTENSION IF NOT EXISTS " + lib + ";"
	switch provider {
	case "rds", "aurora":
		group := "DB parameter group"
		if provider == "aurora" {
			group = "DB cluster parameter group"
		}
		return []string{fmt.Sprintf("In the instance's %s, add %s to "+
			"shared_preload_libraries.", group, lib),
			"Then reboot the instance (the setting applies at start).", create}
	case "cloud-sql":
		return []string{fmt.Sprintf("Cloud SQL preloads %s; no database flag is needed "+
			"to enable it. Its settings are database flags (gcloud sql instances patch "+
			"INSTANCE --database-flags=...).", lib), create}
	case "alloydb":
		return []string{fmt.Sprintf("AlloyDB preloads %s; no database flag is needed to "+
			"enable it. Its settings are database flags (gcloud alloydb instances update "+
			"INSTANCE --database-flags=...).", lib), create}
	case "azure", "azure-cosmos":
		return []string{fmt.Sprintf("In the server parameters, add %s to azure.extensions "+
			"and to shared_preload_libraries (az postgres flexible-server parameter set).",
			strings.ToUpper(lib)), "Restart the server (shared_preload_libraries applies " +
			"at start).", create}
	case "neon", "supabase":
		return []string{fmt.Sprintf("%s is preloaded on %s.", lib, provider), create}
	}
	return []string{fmt.Sprintf("As a superuser: ALTER SYSTEM SET shared_preload_libraries "+
		"= '%s';", withLibrary(preload, lib)),
		"Then restart PostgreSQL: shared_preload_libraries changes only at server start.",
		create}
}

// withLibrary appends lib to the preload list; an unreadable list keeps a
// placeholder the operator fills in.
func withLibrary(preload *string, lib string) string {
	if preload == nil {
		return "<existing libraries>," + lib
	}
	var libs []string
	for _, l := range strings.Split(*preload, ",") {
		if l = strings.TrimSpace(l); l != "" && l != lib {
			libs = append(libs, l)
		}
	}
	return strings.Join(append(libs, lib), ",")
}

func hypoPG(e Extensions, provider string) Capability {
	c := Capability{Name: CapHypoPG, Status: CapabilityOK,
		Detail: "Index recommendations are checked with hypothetical indexes."}
	switch {
	case e.HypoPGInstalled:
		return c
	case e.HypoPGAvailable:
		c.Status = CapabilityMissing
		c.Steps = []string{"CREATE EXTENSION IF NOT EXISTS hypopg; (needs a superuser " +
			"or the provider's admin role)"}
	default:
		c.Status = CapabilityUnavailable
		c.Steps = []string{"Install the hypopg package on the database server (e.g. " +
			"apt install postgresql-<version>-hypopg), then CREATE EXTENSION IF NOT " +
			"EXISTS hypopg;"}
		if provider == "azure" || provider == "azure-cosmos" {
			c.Steps = []string{"Add HYPOPG to the azure.extensions server parameter, " +
				"then CREATE EXTENSION IF NOT EXISTS hypopg;"}
		}
	}
	c.Detail = "Optional: without HypoPG, index recommendations are not checked " +
		"against the planner before they are proposed."
	return c
}

func autoExplain(e Extensions, provider string) Capability {
	c := Capability{Name: CapAutoExplain, Status: CapabilityOK,
		Detail: "Slow statements log their plans."}
	if e.AutoExplainLoaded {
		return c
	}
	c.Status = CapabilityNotLoaded
	c.Detail = "Optional: without auto_explain, plans of slow statements are only " +
		"captured when pg_sage runs EXPLAIN itself."
	switch provider {
	case "rds", "aurora", "azure", "azure-cosmos":
		steps := preloadSteps(provider, "auto_explain", e.PreloadLibraries)
		c.Steps = append(steps[:len(steps)-1], "Set auto_explain.log_min_duration "+
			"(e.g. 1000 ms) in the same parameters.")
	case "cloud-sql", "alloydb":
		c.Steps = []string{"Set the auto_explain database flags (auto_explain." +
			"log_min_duration, e.g. 1000) on the instance."}
	case "neon", "supabase":
		c.Steps = []string{"Enable auto_explain in the " + provider + " project " +
			"settings if your plan offers it (auto_explain.log_min_duration)."}
	default:
		c.Steps = []string{"As a superuser: ALTER SYSTEM SET session_preload_libraries " +
			"= 'auto_explain';", "ALTER SYSTEM SET auto_explain.log_min_duration = " +
			"'1s';", "SELECT pg_reload_conf(); -- new sessions load it, no restart"}
	}
	return c
}
