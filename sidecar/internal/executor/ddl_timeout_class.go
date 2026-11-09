package executor

import (
	"regexp"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// ddlTimeoutClass names the statement-timeout class of one statement
// (executor-05). Only statements that do not block writes for their whole
// run get a long class timeout: a CONCURRENTLY index build (and a
// replacement, which is one), a REINDEX CONCURRENTLY and a plain VACUUM.
// A non-concurrent CREATE INDEX or REINDEX, a VACUUM FULL and every other
// statement keep safety.ddl_timeout_seconds.
type ddlTimeoutClass string

const (
	classDDL        ddlTimeoutClass = "ddl"
	classIndexBuild ddlTimeoutClass = "index_build"
	classReindex    ddlTimeoutClass = "reindex"
	classVacuum     ddlTimeoutClass = "vacuum"
)

var (
	vacuumFullPattern = regexp.MustCompile(`(?i)\bFULL\b`)
	// The CONCURRENTLY keyword must be in the statement's head, so a
	// literal in a blocking build's predicate never earns the long class.
	concurrentBuildHead   = regexp.MustCompile(`^CREATE (UNIQUE )?INDEX CONCURRENTLY `)
	concurrentReindexHead = regexp.MustCompile(
		`^REINDEX (\([^)]*\) )?(INDEX|TABLE|SCHEMA|DATABASE|SYSTEM) CONCURRENTLY `)
)

func timeoutClassOf(sql string) ddlTimeoutClass {
	upper := strings.ToUpper(strings.Join(strings.Fields(normalizeSQLText(sql)), " ")) + " "
	switch {
	case IsIndexReplaceSQL(sql):
		return classIndexBuild
	case concurrentBuildHead.MatchString(upper):
		return classIndexBuild
	case concurrentReindexHead.MatchString(upper):
		return classReindex
	case strings.HasPrefix(upper, "VACUUM") && !vacuumFullPattern.MatchString(upper):
		return classVacuum
	}
	return classDDL
}

// ddlTimeoutFor is the statement timeout of sql: its class timeout, or
// safety.ddl_timeout_seconds for every other statement. It is never
// unlimited.
func (e *Executor) ddlTimeoutFor(sql string) time.Duration {
	safety := e.safetySnapshot()
	switch timeoutClassOf(sql) {
	case classIndexBuild:
		return safety.IndexBuildTimeout()
	case classReindex:
		return safety.ReindexTimeout()
	case classVacuum:
		return safety.VacuumTimeout()
	}
	return e.ddlTimeout()
}

func (e *Executor) safetySnapshot() config.SafetyConfig {
	if cfg, _, _ := e.policySnapshot(); cfg != nil {
		return cfg.Safety
	}
	return config.SafetyConfig{}
}

// applyTimeoutFor bounds each stage of Apply for sql: its statement
// timeout plus a grace period for the server-side timeout to report first.
func (e *Executor) applyTimeoutFor(sql string) time.Duration {
	return e.ddlTimeoutFor(sql) + applyGrace
}

// maxDDLTimeout is the largest statement timeout of any class: the claim,
// lease and budget horizons must outlive the longest run they cover.
func (e *Executor) maxDDLTimeout() time.Duration {
	safety := e.safetySnapshot()
	longest := e.ddlTimeout()
	for _, t := range []time.Duration{
		safety.IndexBuildTimeout(), safety.ReindexTimeout(), safety.VacuumTimeout(),
	} {
		longest = max(longest, t)
	}
	return longest
}

// maxApplyTimeout is maxDDLTimeout plus the Apply grace period.
func (e *Executor) maxApplyTimeout() time.Duration {
	return e.maxDDLTimeout() + applyGrace
}
