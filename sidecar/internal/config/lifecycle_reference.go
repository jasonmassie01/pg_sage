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
	out.WriteString("`restart` is rejected during reload. ")
	out.WriteString("`lifecycle_api` is changed through its dedicated API, ")
	out.WriteString("not YAML reload.\n\n")
	out.WriteString("| Field | Lifecycle | Runtime owner |\n")
	out.WriteString("| --- | --- | --- |\n")
	for _, field := range FieldLifecycles() {
		owner := field.Owner
		if owner == "" {
			owner = "-"
		}
		out.WriteString("| `" + field.Path + "` | `" +
			string(field.Lifecycle) + "` | `" + owner + "` |\n")
	}
	return out.String()
}
