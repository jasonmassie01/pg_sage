package rubric

import (
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/drill"
	"github.com/pg-sage/sidecar/internal/pgversion"
)

// drillResult: a pass in the recoverability window (35 days) passes; an
// older pass warns, or fails when the newest drill failed; drills that
// never passed fail. Until a drill has run the check is unknown, never
// pass.
func drillResult(in DrillInput, now time.Time) CheckResult {
	switch {
	case in.Err != nil:
		return unknown("drill: the restore drill history could not be read: " + in.Err.Error())
	case !in.Attempted:
		return unknown("drill: no restore drill has run for this database yet")
	case in.LastPassedAt.IsZero():
		return CheckResult{Status: StatusFail, Evidence: []string{"drill:none_passed"},
			Reason: "no restore drill has passed"}
	}
	ev := []string{fmt.Sprintf("drill:%d", in.LastPassedID)}
	if now.Sub(in.LastPassedAt) <= drill.RecoverabilityWindow {
		return CheckResult{Status: StatusPass, Evidence: ev}
	}
	if !in.LastAttemptPassed {
		return CheckResult{Status: StatusFail, Evidence: ev,
			Reason: "the newest drill failed and the last pass is older than 35 days"}
	}
	return CheckResult{Status: StatusWarn, Evidence: ev,
		Reason: "the last passed drill is older than 35 days"}
}

func settingResult(in SettingsInput, ref string, now time.Time) CheckResult {
	switch {
	case in.Err != nil:
		return unknown("setting: pg_settings could not be read: " + in.Err.Error())
	case in.ServerVersionNum <= 0:
		return unknown("setting: no server version was read")
	}
	if ref == refPendingRestart {
		ev := []string{"setting:pg_settings.pending_restart"}
		if len(in.PendingRestart) > 0 {
			return CheckResult{Status: StatusWarn, Evidence: ev, Reason: "waiting for a " +
				"restart: " + strings.Join(in.PendingRestart, ", ")}
		}
		return CheckResult{Status: StatusPass, Evidence: ev}
	}
	return eolResult(in.ServerVersionNum/10000, now)
}

// eolResult: past the community end of life fails; within 90 days of it
// warns; a major pg_sage has no date for is unknown (older than the table
// is past end of life).
func eolResult(major int, now time.Time) CheckResult {
	ev := []string{fmt.Sprintf("setting:server_version_num major %d", major)}
	date, ok := pgversion.EOLDate(major)
	if !ok {
		if major < pgversion.OldestKnownMajor() {
			return CheckResult{Status: StatusFail, Evidence: ev,
				Reason: fmt.Sprintf("PostgreSQL %d is past end of life", major)}
		}
		return unknown(fmt.Sprintf("setting: pg_sage has no end-of-life date for "+
			"PostgreSQL %d", major))
	}
	end, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return unknown("setting: end-of-life date " + date + " does not parse")
	}
	switch {
	case !now.Before(end):
		return CheckResult{Status: StatusFail, Evidence: ev,
			Reason: fmt.Sprintf("PostgreSQL %d reached end of life on %s", major, date)}
	case pgversion.NearEOL(major, now):
		return CheckResult{Status: StatusWarn, Evidence: ev,
			Reason: fmt.Sprintf("PostgreSQL %d reaches end of life on %s", major, date)}
	}
	return CheckResult{Status: StatusPass, Evidence: ev}
}

// upgradeResult grades the upgrade readiness case, which the analyzer runs
// only for a major at or near end of life: for any other major there is
// no upgrade target, so the check does not apply.
func upgradeResult(in Inputs, now time.Time) CheckResult {
	s := in.Settings
	switch {
	case s.Err != nil:
		return unknown("upgrade: pg_settings could not be read: " + s.Err.Error())
	case s.ServerVersionNum <= 0:
		return unknown("upgrade: no server version was read")
	}
	major := s.ServerVersionNum / 10000
	if !pgversion.NearEOL(major, now) {
		return CheckResult{Status: StatusNotApplicable, Reason: fmt.Sprintf("PostgreSQL %d "+
			"is not near end of life: no upgrade target", major)}
	}
	return findingResult(in.Findings, "upgrade_readiness")
}
