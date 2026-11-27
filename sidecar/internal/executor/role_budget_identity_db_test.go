package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// security-09 x market-08 on a live server: with a separate action
// identity, a role budget change and its rollback run as the action role.
// The observation role cannot ALTER ROLE, so on one identity the server
// refuses the same change.
func TestActionPool_RoleBudgetRunsAsActionRole(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	db := budgetRoleSetup(t, f.super)
	for _, s := range []string{"ALTER ROLE " + f.actor + " CREATEROLE",
		"GRANT " + budgetRole + " TO " + f.actor + " WITH ADMIN OPTION"} {
		if _, err := f.super.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	sql := `ALTER ROLE "` + budgetRole + `" IN DATABASE ` + pgx.Identifier{db}.Sanitize() +
		` SET statement_timeout = '6000ms'`
	if _, err := runRoleBudget(t, roleBudgetExecutor(f.obs), sql); err == nil {
		t.Fatal("the observation role changed a role budget; want the server to refuse it")
	}
	if got := roleSettings(t, f.super); got != "" {
		t.Fatalf("refused change left settings %q", got)
	}
	split := roleBudgetExecutor(f.obs).WithActionPool(f.act)
	change, err := runRoleBudget(t, split, sql)
	if err != nil {
		t.Fatalf("role budget as the action role: %v", err)
	}
	if got := roleSettings(t, f.super); !strings.HasSuffix(got, ":statement_timeout=6000ms") {
		t.Fatalf("settings after the change = %q, want statement_timeout=6000ms", got)
	}
	if _, err := runRoleBudget(t, split, change.rollbackSQL); err != nil {
		t.Fatalf("rollback as the action role: %v", err)
	}
	if got := roleSettings(t, f.super); got != "" {
		t.Fatalf("settings after rollback = %q, want none", got)
	}
}
