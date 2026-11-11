package value

import "testing"

// earned-04: every modelled class lands on a seeded, active toil model,
// and the credit still prices legacy action names by their model.

func TestToilActionTypeForClassLandsOnAnActiveSeededModel(t *testing.T) {
	pool, ctx := requireValuePostgres(t)
	for _, class := range []string{"index_create", "index_drop", "reindex", "vacuum",
		"analyze", "freeze", "autovacuum_tuning", "statistics", "query_hint",
		"config_guc"} {
		var minutes float64
		err := pool.QueryRow(ctx, `SELECT base_minutes::float8 FROM sage.toil_model
			WHERE action_type=$1 AND effective_to IS NULL`,
			ToilActionTypeForClass(class)).Scan(&minutes)
		if err != nil || minutes <= 0 {
			t.Errorf("class %s (%s): minutes=%v err=%v", class,
				ToilActionTypeForClass(class), minutes, err)
		}
	}
}

func TestCreditCandidatePricesLegacyAndUnknownActionTypes(t *testing.T) {
	pool, ctx := requireValuePostgres(t)
	repo := NewPostgresRepository(pool)
	databaseID, _ := insertValueDatabase(t, ctx, pool)
	cases := []struct {
		actionType string
		minutes    float64
		version    int
	}{
		{"create_index", 45, 1},
		{"drop_index", 20, 1},
		{"freeze_table", 20, 1},
		{"mystery_action", 0, 0},
	}
	for _, c := range cases {
		graph := insertValueEvidenceGraph(t, ctx, pool, databaseID, "toil-"+c.actionType)
		if _, err := pool.Exec(ctx, `UPDATE sage.action_log SET action_type=$2
			WHERE id=$1`, graph.actionID, c.actionType); err != nil {
			t.Fatalf("retype action: %v", err)
		}
		got, err := repo.CreditCandidate(ctx, graph.actionID)
		if err != nil {
			t.Fatalf("CreditCandidate(%s): %v", c.actionType, err)
		}
		if got.ActionType != c.actionType || got.ModelMinutes != c.minutes ||
			got.ModelVersion != c.version || got.VerificationState != "verified" ||
			got.Outcome != "success" || got.ActionID != graph.actionID {
			t.Errorf("CreditCandidate(%s) = %#v, want %v min v%d", c.actionType, got,
				c.minutes, c.version)
		}
	}
}
