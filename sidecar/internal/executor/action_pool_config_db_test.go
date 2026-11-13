package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// A server setting change is reloaded by the role that made it: with a
// separate action identity, pg_reload_conf runs on the action pool, so an
// observation role without EXECUTE on it does not leave the new value
// written to postgresql.auto.conf but never reloaded (security-09).
func TestActionPool_ConfigChangeReloadsAsActionRole(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	grant := "GRANT EXECUTE ON FUNCTION pg_catalog.pg_reload_conf() TO " + f.actor
	if _, err := f.super.Exec(ctx, grant); err != nil {
		t.Fatalf("%s: %v", grant, err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = f.super.Exec(bg, "ALTER SYSTEM RESET work_mem")
		_, _ = f.super.Exec(bg, "SELECT pg_reload_conf()")
		_, _ = f.super.Exec(bg, "REVOKE EXECUTE ON FUNCTION pg_catalog.pg_reload_conf() FROM "+
			f.actor)
	})
	var ok bool
	err := f.obs.QueryRow(ctx, "SELECT pg_reload_conf()").Scan(&ok)
	requireSQLState(t, "pg_reload_conf as the observation role", err, "42501")

	split := New(f.obs, config.DefaultConfig(), time.Time{}, nopLog).WithActionPool(f.act)
	split.settingWait = 10 * time.Second
	sql := "ALTER SYSTEM SET work_mem = '4321kB'"
	change, err := split.prepareConfigChange(ctx, sql)
	if err != nil || change == nil || change.kind != configKindGUC {
		t.Fatalf("prepareConfigChange(%q) = %+v, %v; want a GUC change", sql, change, err)
	}
	// ALTER SYSTEM itself needs superuser or a per-parameter grant (15+);
	// what this test pins is the reload and read-back that follow it.
	if _, err := f.super.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	state, note, _ := split.readBackConfig(ctx, change)
	if strings.Contains(note, "pg_reload_conf failed") {
		t.Fatalf("read-back reloaded on the observation role: %s", note)
	}
	if state != readbackInEffect {
		t.Fatalf("read-back state = %s (%s), want %s", state, note, readbackInEffect)
	}
}
