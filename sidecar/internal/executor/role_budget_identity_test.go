package executor

import (
	"errors"
	"strings"
	"testing"
)

// security-09 x market-08: with a separate action identity the role budget
// precheck reads session_user on the action pool, so pg_sage's monitoring
// role must still be refused by name.
// No concurrent access tests: checkRoleBudget reads fixed facts once.
// lazyPool is action_pool_test.go's never-connected pool.

func TestRoleBudget_RefusesBothPgSageIdentities(t *testing.T) {
	timeout, ok := parseRoleBudgetSQL(
		`ALTER ROLE "sage_monitor" IN DATABASE "shop" SET statement_timeout = '6000ms'`)
	if !ok {
		t.Fatal("timeout statement did not parse")
	}
	// The action identity's session_user is what the precheck reads.
	facts := roleFacts{exists: true, self: "sage_act", database: "shop"}
	split := &Executor{pool: lazyPool(t, "sage_monitor"), actionPool: lazyPool(t, "sage_act")}
	if got := split.monitoringRole(); got != "sage_monitor" {
		t.Fatalf("monitoringRole() = %q, want sage_monitor", got)
	}
	err := split.checkRoleBudget(timeout, facts)
	if !errors.Is(err, ErrRoleBudgetRefused) || !strings.Contains(err.Error(), "monitoring") {
		t.Fatalf("budget on the monitoring role: %v, want refused as monitoring role", err)
	}
	actor := timeout
	actor.Role = "sage_act"
	err = split.checkRoleBudget(actor, facts)
	if !errors.Is(err, ErrRoleBudgetRefused) || !strings.Contains(err.Error(), "own role") {
		t.Fatalf("budget on the action role: %v, want refused as pg_sage's own role", err)
	}
	app := timeout
	app.Role = "app"
	if err := split.checkRoleBudget(app, facts); err != nil {
		t.Fatalf("budget on an application role: %v, want allowed", err)
	}
}

// Nil/empty: one identity (no action pool) leaves the name check to
// session_user, and no pool at all has no monitoring role.
func TestRoleBudget_MonitoringRoleWithoutActionPool(t *testing.T) {
	single := &Executor{pool: lazyPool(t, "sage_monitor")}
	if got := single.monitoringRole(); got != "" {
		t.Fatalf("single identity monitoringRole() = %q, want empty", got)
	}
	if got := (&Executor{}).monitoringRole(); got != "" {
		t.Fatalf("no pool monitoringRole() = %q, want empty", got)
	}
	if got := (&Executor{actionPool: lazyPool(t, "sage_act")}).actionDB().Config().
		ConnConfig.User; got != "sage_act" {
		t.Fatalf("actionDB() user = %q, want the action identity", got)
	}
}
