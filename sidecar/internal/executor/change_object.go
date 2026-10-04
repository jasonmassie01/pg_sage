package executor

import (
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

// The identity one change per object serializes on, from the statement
// alone (never from evidence or model text): a settings change is its GUC
// ("guc:work_mem", whatever its scope: system, database or role); every
// other change is the table it touches ("table:public.memories"), an
// index counting as its table (policy.ResolveChangeTables).

// changeGUC is the lower-case name of the setting a statement changes, or
// "" when it changes none (RESET ALL changes every setting and names none).
func changeGUC(sql string) string {
	if stmt, ok := pgconf.ParseAlterSystem(sql); ok {
		return guardGUCName(stmt.Name)
	}
	fields := strings.Fields(strings.TrimRight(normalizeSQLText(sql), "; "))
	if len(fields) < 5 || !strings.EqualFold(fields[0], "ALTER") {
		return ""
	}
	switch strings.ToUpper(fields[1]) {
	case "DATABASE", "ROLE", "USER":
	default:
		return ""
	}
	for i := 3; i < len(fields)-1; i++ {
		switch strings.ToUpper(fields[i]) {
		case "SET", "RESET":
			name, _, _ := strings.Cut(fields[i+1], "=")
			return guardGUCName(strings.ToLower(strings.Trim(name, `"`)))
		}
	}
	return ""
}

// guardGUCName drops names that are not one setting.
func guardGUCName(name string) string {
	switch name {
	case "", "all", "tablespace":
		return ""
	}
	return name
}

// settingsStatement reports ALTER SYSTEM / DATABASE / ROLE / USER: they
// name no relation, whatever target the finding carries ("instance").
func settingsStatement(sql string) bool {
	fields := strings.Fields(strings.ToUpper(normalizeSQLText(sql)))
	if len(fields) < 2 || fields[0] != "ALTER" {
		return false
	}
	switch fields[1] {
	case "SYSTEM", "DATABASE", "ROLE", "USER":
		return true
	}
	return false
}

// changeRelationNames are the relations a change names, as written: the
// statement's own target (statementTarget, the operator lease's reading)
// and the request's targets, a recommendation identity
// "<schema>.<table>|<definition>" counting as its table.
func changeRelationNames(sql string, targets []string) []string {
	if settingsStatement(sql) {
		return nil
	}
	var names []string
	seen := map[string]bool{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, name := range operatorLeaseTargets(sql) {
		add(name)
	}
	// Any CREATE STATISTICS reads one table, also in forms the strict
	// pg_sage parser behind statementTarget refuses (the verifier's reading).
	add(statisticsTable(sql))
	for _, target := range targets {
		table, _, _ := strings.Cut(target, "|")
		add(table)
	}
	return names
}

// VerificationWindows bound how long a change waits for another change's
// verification: the first window of each kind, the cap evidence may extend
// it to, a grace for the last check to land, and how long an authorized
// change that has not run yet holds its object.
type VerificationWindows struct {
	RollbackWindow time.Duration
	CreateWindow   time.Duration
	Cap            time.Duration
	Grace          time.Duration
	HoldHorizon    time.Duration
}

// verificationGrace lets the last verification check after the cap land.
const verificationGrace = time.Hour

// verificationWindowsFor reads the windows from cfg; unset values take the
// documented defaults, never a zero-length wait.
func verificationWindowsFor(cfg *config.Config, holdHorizon time.Duration) VerificationWindows {
	w := VerificationWindows{
		RollbackWindow: minutes(config.DefaultRollbackWindowMinutes),
		CreateWindow:   minutes(config.DefaultVerifyWindowMinutes),
		Cap:            minutes(config.DefaultVerifyWindowMaxMinutes),
		Grace:          verificationGrace, HoldHorizon: holdHorizon,
	}
	if w.HoldHorizon <= 0 {
		w.HoldHorizon = 2 * (time.Duration(config.DefaultDDLTimeoutSeconds)*time.Second +
			applyGrace)
	}
	if cfg == nil {
		return w
	}
	positive(&w.RollbackWindow, minutes(cfg.Trust.RollbackWindowMinutes))
	positive(&w.CreateWindow, minutes(cfg.Verify.WindowMinutes))
	positive(&w.Cap, minutes(cfg.Verify.WindowMaxMinutes))
	return w
}

func minutes(n int) time.Duration { return time.Duration(n) * time.Minute }

func positive(target *time.Duration, value time.Duration) {
	if value > 0 {
		*target = value
	}
}

// waitTimes are when the verification of a change executed at executedAt
// is next due (its first window's end, or the hard deadline once that has
// passed) and when the wait ends without a verdict. An index drop holds
// its table only until its first window concludes (owner decision, PR
// #122): its soft-drop monitoring watches the business cycle on its own.
// Anything else may be extended to the cap, plus the grace.
func waitTimes(sql string, executedAt, now time.Time, w VerificationWindows) (
	until, hard time.Time) {
	first, last := w.RollbackWindow, max(w.Cap, w.RollbackWindow)+w.Grace
	switch verificationClass(sql) {
	case verify.ClassIndexDrop:
		first, last = w.RollbackWindow, w.RollbackWindow
	case verify.ClassIndexCreate:
		first, last = w.CreateWindow, max(w.Cap, w.CreateWindow)+w.Grace
	}
	until, hard = executedAt.Add(first), executedAt.Add(last)
	if !now.Before(until) {
		until = hard
	}
	return until, hard
}

// waitRelease is why a change's wait ends without a verdict.
func waitRelease(sql string) string {
	if verificationClass(sql) == verify.ClassIndexDrop {
		return policy.ReleaseDropFirstWindow
	}
	return policy.ReleaseHardDeadline
}

// partitionScoped reports a change that shares one object across a
// partition tree (owner decision, PR #122): index, extended statistics and
// reloption changes move the plans of every partition. VACUUM, ANALYZE
// and settings keep their own object.
func partitionScoped(sql string) bool {
	switch verificationClass(sql) {
	case verify.ClassIndexCreate, verify.ClassIndexDrop, verify.ClassReindex,
		verify.ClassStatistics, verify.ClassReloption:
		return true
	}
	return false
}
