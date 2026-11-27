package main

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/doctor"
)

// startupDoctorChecks are the doctor checks the startup line counts: the
// SQL checks on the monitored database. The model probe and the test
// notification are never sent at startup, and the meta database is
// checked by its own start-up path.
var startupDoctorChecks = map[string]bool{
	doctor.IDConnection: true, doctor.IDVersion: true, doctor.IDRoleGrants: true,
	doctor.IDExtensions: true, doctor.IDProvider: true, doctor.IDQueryText: true,
	doctor.IDClockSkew: true,
}

// logDoctorCounts logs, once per database at startup, the doctor report's
// counts (ux-04a): INFO when nothing failed, else one WARN naming the
// failed checks. pg_sage doctor prints each fix.
func logDoctorCounts(ctx context.Context, pool *pgxpool.Pool, name string,
	info, warn func(component, msg string, args ...any)) {
	if pool == nil {
		return
	}
	r := doctor.Run(ctx, doctor.Inputs{Target: pool, ModelSkipped: "not probed at startup"})
	var checks []doctor.Check
	var failed []string
	for _, c := range r.Checks {
		if !startupDoctorChecks[c.ID] {
			continue
		}
		checks = append(checks, c)
		if c.Status == doctor.StatusFail {
			failed = append(failed, c.ID)
		}
	}
	n := doctor.Count(checks)
	msg := "db %q: doctor: %d pass, %d warn, %d fail, %d unknown (provider %s)"
	args := []any{name, n.Pass, n.Warn, n.Fail, n.Unknown, r.Provider}
	if len(failed) == 0 {
		info("runtime", msg+"; pg_sage doctor --pg-url <dsn> lists each fix", args...)
		return
	}
	warn("runtime", msg+"; failed: %s; pg_sage doctor --pg-url <dsn> lists each fix",
		append(args, strings.Join(failed, ", "))...)
}
