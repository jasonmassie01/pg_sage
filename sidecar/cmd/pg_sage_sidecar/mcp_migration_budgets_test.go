package main

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	rehearsalpkg "github.com/pg-sage/sidecar/internal/migration/rehearsal"
	"github.com/pg-sage/sidecar/internal/testsupport/parsetree"
)

// No concurrent access tests: both helpers read a config value once and
// return a fresh value.

// migration-06: the loaded config's budgets reach the rehearsal and the
// planner; without a config the package defaults apply.
func TestMigrationBudgetsFollowConfig(t *testing.T) {
	parsetree.Require(t)
	loaded := config.DefaultConfig()
	loaded.Rehearsal.PlanFlipPct = 3
	loaded.Migration.MaxLockMS = 750
	loaded.Migration.ContractDelayMinutes = 120
	options := migrationRehearsalOptions(loaded, time.Hour)
	if options.PlanFlipPct != 3 || options.MaxLock != 750*time.Millisecond ||
		options.StatementTimeout != 10*time.Minute || options.MaxCloneAge != time.Hour ||
		options.RegressionPct != 15 {
		t.Fatalf("options = %#v", options)
	}
	planned, err := migrationPlanner(loaded).Plan(context.Background(), planpkg.Request{
		SQL:   "ALTER TABLE public.users ALTER COLUMN email SET NOT NULL",
		Table: planpkg.TableFacts{Schema: "public", Name: "users"},
	})
	if err != nil || planned.ContractNotBefore != 2*time.Hour {
		t.Fatalf("plan delay = %s err=%v, want 2h", planned.ContractNotBefore, err)
	}
}

func TestMigrationBudgetsWithoutConfigUseDefaults(t *testing.T) {
	parsetree.Require(t)
	options := migrationRehearsalOptions(nil, time.Hour)
	if options.PlanFlipPct != 0 || options.MaxLock != 0 ||
		options.StatementTimeout != rehearsalpkg.DefaultStepStatementTimeout {
		t.Fatalf("options = %#v (zero budgets mean the rehearsal defaults)", options)
	}
	planned, err := migrationPlanner(nil).Plan(context.Background(), planpkg.Request{
		SQL:   "ALTER TABLE public.users ALTER COLUMN email SET NOT NULL",
		Table: planpkg.TableFacts{Schema: "public", Name: "users"},
	})
	if err != nil || planned.ContractNotBefore != time.Hour {
		t.Fatalf("plan delay = %s err=%v, want 1h", planned.ContractNotBefore, err)
	}
}
