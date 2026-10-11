package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// The startup self-check of spec §6.15 (AP-14): pg_sage's role must not be
// a superuser for Agent Guard features. It is one warning line; a
// non-superuser role adds nothing. No concurrent access tests:
// reportGuardRole is stateless.

func TestReportGuardRoleSuperuserWarns(t *testing.T) {
	var lines []logLine
	reportGuardRole("postgres", true, captureLog(&lines))
	if len(lines) != 1 {
		t.Fatalf("lines = %+v, want one", lines)
	}
	l := lines[0]
	for _, want := range []string{"WARNING", `"postgres"`, "superuser", "Agent Guard",
		"AP-14", "posture"} {
		if l.component != "grants" || !strings.Contains(l.text, want) {
			t.Fatalf("line = %+v, want it to contain %q", l, want)
		}
	}
}

func TestReportGuardRoleNonSuperuserSilent(t *testing.T) {
	var lines []logLine
	reportGuardRole("sage_agent", false, captureLog(&lines))
	if len(lines) != 0 {
		t.Fatalf("lines = %+v, want none", lines)
	}
}

// Integration: the test servers connect as a superuser, so VerifyGrants
// logs the Guard warning once, whatever the trust level.
func TestVerifyGrantsWarnsSuperuserForGuard(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	var super bool
	if err := pool.QueryRow(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname = current_user").
		Scan(&super); err != nil || !super {
		t.Skipf("the test DSN is not a superuser (%v): nothing to warn about", err)
	}
	for _, level := range []string{"observation", "advisory"} {
		var lines []logLine
		VerifyGrants(ctx, pool, "ignored", level, captureLog(&lines))
		n := 0
		for _, l := range lines {
			if strings.Contains(l.text, "Agent Guard") {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%s: lines = %+v, want one Agent Guard warning", level, lines)
		}
	}
}
