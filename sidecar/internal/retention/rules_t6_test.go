package retention

import (
	"context"
	"strings"
	"testing"
)

// T6 round 1 (review-02f, review-05a): the capacity projections and the
// model-gate evaluations age out after 400 days; a gate row with an
// approval or a revocation is kept. No concurrent access tests: purgeRules
// is a pure function of the config and Run's batches are tested elsewhere.

func TestPurgeRules_T6ForecastAndGateKept400Days(t *testing.T) {
	for _, days := range []int{0, 7, 30} {
		found := map[string][]purgeRule{}
		for _, r := range purgeRules(allDays(days)) {
			if r.table == "review_capacity_forecast" || r.table == "review_gate" {
				found[r.table] = append(found[r.table], r)
			}
		}
		f, g := found["review_capacity_forecast"], found["review_gate"]
		if len(f) != 1 || f[0].timeCol != "created_at" || f[0].days != 400 ||
			f[0].extra != "" {
			t.Fatalf("days %d: review_capacity_forecast rules %+v, want one on created_at "+
				"for 400 days", days, f)
		}
		if len(g) != 1 || g[0].timeCol != "evaluated_at" || g[0].days != 400 ||
			g[0].extra != "AND approved_by IS NULL AND revoked_at IS NULL" {
			t.Fatalf("days %d: review_gate rules %+v, want one on evaluated_at for 400 "+
				"days that keeps approvals and revocations", days, g)
		}
	}
}

// Every T6 exemption has a reason, and no table is both exempt and ruled.
func TestT6Exemptions_HaveReasonsAndNoRule(t *testing.T) {
	ruled := map[string]bool{}
	for _, r := range purgeRules(allDays(30)) {
		ruled[r.table] = true
	}
	if len(trancheSixExemptions) != 12 {
		t.Fatalf("%d T6 exemptions, want 12", len(trancheSixExemptions))
	}
	for table, why := range trancheSixExemptions {
		if len(strings.TrimSpace(why)) < 20 || ruled[table] ||
			retentionExemptions[table] != why {
			t.Errorf("exemption %s: reason %q, ruled %t, merged %t", table, why,
				ruled[table], retentionExemptions[table] == why)
		}
	}
}

// Integration: a real Run deletes an old unapproved gate row and an old
// projection, and keeps the old approved and revoked rows and fresh rows.
func TestRun_T6GateKeepsApprovalsAndForecastAgesOut(t *testing.T) {
	_, ctx := requireDB(t)
	model := uniqueTag("gate_ret")
	insert := func(tick, age, verdict string, approved, revoked bool) int64 {
		var id int64
		err := testPool.QueryRow(ctx, `INSERT INTO sage.review_gate (model, rule, tick,
			evaluated_at, verdict, reason, approved_by, approved_at, revoked_at)
			VALUES ($1, 'G-M1', now() - $2::interval, now() - $3::interval, $4, 'test',
			        CASE WHEN $5 THEN 'ops' END, CASE WHEN $5 THEN now() END,
			        CASE WHEN $6 THEN now() END)
			RETURNING id`, model, tick, age, verdict, approved, revoked).Scan(&id)
		if err != nil {
			t.Fatalf("insert gate row %s: %v", verdict, err)
		}
		return id
	}
	oldPlain := insert("500 days", "500 days", "eligible", false, false)
	oldGranted := insert("499 days", "499 days", "granted", true, false)
	oldRevoked := insert("498 days", "498 days", "revoked", false, true)
	fresh := insert("1 day", "1 day", "eligible", false, false)
	if _, err := testPool.Exec(ctx, `INSERT INTO sage.review_capacity_forecast
		(database_name, made_on, target_on, projected_bytes, band_bytes, created_at)
		VALUES ($1, current_date - 500, current_date - 470, 100, 10,
		        now() - interval '500 days'),
		       ($1, current_date - 1, current_date + 29, 100, 10,
		        now() - interval '1 day')`, model); err != nil {
		t.Fatalf("insert projections: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = testPool.Exec(bg, `DELETE FROM sage.review_gate WHERE model = $1`, model)
		_, _ = testPool.Exec(bg, `DELETE FROM sage.review_capacity_forecast
			WHERE database_name = $1`, model)
	})
	New(testPool, allDays(30), noopLog).Run(ctx)
	for id, kept := range map[int64]bool{oldPlain: false, oldGranted: true,
		oldRevoked: true, fresh: true} {
		n := countWhere(t, ctx, `SELECT count(*) FROM sage.review_gate WHERE id = $1`, id)
		if (n == 1) != kept {
			t.Errorf("gate row %d: %d left, kept want %t", id, n, kept)
		}
	}
	n := countWhere(t, ctx, `SELECT count(*) FROM sage.review_capacity_forecast
		WHERE database_name = $1 AND made_on = current_date - 1`, model)
	old := countWhere(t, ctx, `SELECT count(*) FROM sage.review_capacity_forecast
		WHERE database_name = $1 AND made_on = current_date - 500`, model)
	if n != 1 || old != 0 {
		t.Fatalf("projections: fresh %d, old %d; want 1 and 0", n, old)
	}
}
