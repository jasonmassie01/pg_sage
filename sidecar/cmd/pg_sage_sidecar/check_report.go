package main

import "github.com/pg-sage/sidecar/internal/startup"

// reportChecks logs what the prerequisite checks leave degraded: a failed
// check, query text pg_sage cannot read, and each capability that could
// not be checked, with its cause and its effect.
func reportChecks(name string, checks *startup.CheckResult, err error,
	warn func(component, msg string, args ...any)) {
	if err != nil {
		warn("runtime", "db %q: prerequisite checks failed; monitoring "+
			"degraded: %v", name, err)
		return
	}
	if !checks.QueryTextVisible {
		warn("runtime", "db %q: other roles' query text is hidden: the monitoring "+
			"role does not inherit pg_read_all_stats; GRANT pg_read_all_stats to "+
			"the monitoring role (and keep it INHERIT)", name)
	}
	for _, w := range checks.CapabilityWarnings() {
		warn("runtime", "db %q: %s", name, w)
	}
}
