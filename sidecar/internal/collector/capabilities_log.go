package collector

import (
	"fmt"
	"strings"
)

// storeCapabilities installs the capabilities of key and says once what
// changed: a recovered probe, a created, dropped or updated extension, or a
// new server version.
func (c *Collector) storeCapabilities(key capKey, next Capabilities, cached bool) {
	c.mu.Lock()
	prev := c.caps
	c.caps = capState{caps: next, key: key, attempted: true, probed: true, cached: cached}
	c.mu.Unlock()
	if prev.failing {
		c.logFn("INFO", "collector: capability probe recovered")
	}
	if capabilitiesChanged(prev, next) {
		level, msg := describeCapabilities(next)
		c.logFn(level, "%s", msg)
	}
}

// capabilityProbeSucceeded ends a failure streak on an unchanged key.
func (c *Collector) capabilityProbeSucceeded() {
	c.mu.Lock()
	recovered := c.caps.failing
	c.caps.failing = false
	c.mu.Unlock()
	if recovered {
		c.logFn("INFO", "collector: capability probe recovered")
	}
}

// capabilityProbeFailed keeps the last capabilities and warns once per
// failure streak.
func (c *Collector) capabilityProbeFailed(what string, err error) {
	c.mu.Lock()
	warn := !c.caps.failing
	c.caps.failing = true
	version := c.caps.caps.PGVersionNum
	c.mu.Unlock()
	if warn {
		c.logFn("WARN", "collector: capability probe (%s) failed; keeping the last "+
			"capabilities (server %d) and retrying every cycle: %v", what, version, err)
	}
}

// capabilitiesChanged reports whether next is news. The first probe is
// compared with the startup seed only on what the startup checks know.
func capabilitiesChanged(prev capState, next Capabilities) bool {
	if prev.probed {
		return next != prev.caps
	}
	seed := prev.caps
	return next.WALColumns != seed.WALColumns ||
		next.PlanTimeColumns != seed.PlanTimeColumns ||
		(seed.PGVersionNum != 0 && next.PGVersionNum != seed.PGVersionNum)
}

// describeCapabilities is the one line logged when the capabilities change.
func describeCapabilities(caps Capabilities) (string, string) {
	if !caps.StatStatementsView {
		return "WARN", fmt.Sprintf("collector: pg_stat_statements not found (server %d); "+
			"query statistics are unavailable until CREATE EXTENSION pg_stat_statements "+
			"is run in this database (no pg_sage restart needed)", caps.PGVersionNum)
	}
	version := caps.StatStatementsVersion
	if version == "" {
		version = "(unknown version)"
	}
	var cols []string
	if caps.WALColumns {
		cols = append(cols, "WAL")
	}
	if caps.PlanTimeColumns {
		cols = append(cols, "plan time")
	}
	if caps.BlockIOTime {
		cols = append(cols, "block I/O")
	}
	what := "only the base counters are collected (no WAL, plan time or block I/O columns)"
	if len(cols) > 0 {
		what = joinAnd(cols) + " now collected"
	}
	return "INFO", fmt.Sprintf("collector: pg_stat_statements %s detected (server %d); %s",
		version, caps.PGVersionNum, what)
}

// joinAnd joins words as "a, b and c".
func joinAnd(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}
