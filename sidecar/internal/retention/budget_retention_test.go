package retention

import (
	"context"
	"fmt"
	"testing"
)

// market-08's tables age by their UTC day, which is indexed: updated_at
// moves on every upsert and is not indexed, so the upserts stay HOT.

func TestPurgeRules_BudgetTablesAgeByDay(t *testing.T) {
	found := map[string]bool{}
	for _, rule := range purgeRules(allDays(30)) {
		switch rule.table {
		case "budget_shadow", "workload_role_daily":
			if rule.timeCol != "day" || rule.days != 30 || rule.extra != "" {
				t.Fatalf("%s rule = %+v, want day over 30 days", rule.table, rule)
			}
			found[rule.table] = true
		}
	}
	if !found["budget_shadow"] || !found["workload_role_daily"] {
		t.Fatalf("budget rules found: %v", found)
	}
}

func TestRun_BudgetRowsAgeOutByDay(t *testing.T) {
	_, ctx := requireDB(t)
	db := uniqueTag("budget_ret")
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = testPool.Exec(bg, `DELETE FROM sage.workload_role_daily WHERE database_name = $1`, db)
		_, _ = testPool.Exec(bg, `DELETE FROM sage.budget_shadow WHERE database_name = $1`, db)
	})
	// Ages in days; updated_at is now() for every row (a late upsert of an
	// old day must not keep it past its day's window).
	for _, age := range []int{0, 29, 31, 400} {
		if _, err := testPool.Exec(ctx, `INSERT INTO sage.workload_role_daily
			(database_name, role_name, day, calls) VALUES ($1, $2, current_date - $3::int, 1)`,
			db, fmt.Sprintf("r%d", age), age); err != nil {
			t.Fatalf("insert role day %d: %v", age, err)
		}
		if _, err := testPool.Exec(ctx, `INSERT INTO sage.budget_shadow
			(database_name, budget, day) VALUES ($1, $2, current_date - $3::int)`,
			db, fmt.Sprintf("b%d", age), age); err != nil {
			t.Fatalf("insert shadow day %d: %v", age, err)
		}
	}
	New(testPool, allDays(30), noopLog).Run(ctx)
	for _, table := range []string{"workload_role_daily", "budget_shadow"} {
		var kept []int
		rows, err := testPool.Query(ctx, `SELECT (current_date - day)::int FROM sage.`+table+
			` WHERE database_name = $1 ORDER BY 1`, db)
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		for rows.Next() {
			var age int
			if err := rows.Scan(&age); err != nil {
				t.Fatalf("scan %s: %v", table, err)
			}
			kept = append(kept, age)
		}
		rows.Close()
		if len(kept) != 2 || kept[0] != 0 || kept[1] != 29 {
			t.Fatalf("%s kept days %v ago, want [0 29]", table, kept)
		}
	}
}
