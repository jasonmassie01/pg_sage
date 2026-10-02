package config

import "strings"

// ConfigLifecycleMarkdown renders the public per-field lifecycle reference.
func ConfigLifecycleMarkdown() string {
	var out strings.Builder
	out.WriteString("# Configuration field lifecycles\n\n")
	out.WriteString("> Generated from `internal/config` by `cmd/gen_config_meta`; ")
	out.WriteString("do not edit manually.\n\n")
	out.WriteString("`live_policy` swaps an immutable policy snapshot. ")
	out.WriteString("`reconfigure` tears down and rebuilds the named owner. ")
	out.WriteString("`restart` is rejected during reload. A `reconfigure` or ")
	out.WriteString("`live_policy` field whose owner is not running in the ")
	out.WriteString("current mode is treated as `restart`.\n\n")
	writeLifecycleTable(&out, FieldLifecycles())
	out.WriteString("\n## Per-database fields (`databases[]`)\n\n")
	out.WriteString("In YAML fleet mode the `fleet_databases` owner reconciles ")
	out.WriteString("the list: an added entry starts a runtime, a removed entry ")
	out.WriteString("drains and stops one, and a renamed entry is a removal plus ")
	out.WriteString("an addition. `live_policy` fields apply in place to the ")
	out.WriteString("running database; `reconfigure` fields rebuild only that ")
	out.WriteString("database's runtime. Meta-db mode applies the same classes ")
	out.WriteString("to `sage.databases` rows.\n\n")
	writeLifecycleTable(&out, DatabaseFieldLifecycles())
	return out.String()
}

func writeLifecycleTable(out *strings.Builder, fields []FieldLifecycle) {
	out.WriteString("| Field | Lifecycle | Runtime owner |\n")
	out.WriteString("| --- | --- | --- |\n")
	for _, field := range fields {
		owner := field.Owner
		if owner == "" {
			owner = "-"
		}
		out.WriteString("| `" + field.Path + "` | `" +
			string(field.Lifecycle) + "` | `" + owner + "` |\n")
	}
}
