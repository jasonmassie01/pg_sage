package earned

import (
	"context"
	"testing"
)

// An approval card shows the trust of the class its queued action belongs
// to: the pair an operator's decision on it is recorded under.

func TestPairForQueuedAction(t *testing.T) {
	for _, tc := range []struct {
		name, actionType, sql, identity string
		family                          Family
		class                           ActionClass
	}{
		{"analyze", "analyze_table", "ANALYZE public.orders", "", FamilyHygiene,
			ClassAnalyze},
		{"index create", "create_index_concurrently",
			"CREATE INDEX CONCURRENTLY i ON public.orders (a)", "", FamilyTuning,
			ClassIndexCreate},
		{"guc", "alter_system_guc", "ALTER SYSTEM SET work_mem = '64MB'", "",
			FamilyTuning, ClassConfigGUC},
		{"incident handoff", "vacuum_table", "VACUUM (FREEZE) public.orders",
			HandoffKey(FamilyWraparound, ClassFreeze, []string{"public.orders"}),
			FamilyWraparound, ClassFreeze},
		{"self handoff", "analyze_table", "ANALYZE public.orders",
			"autonomy:hygiene:analyze:public.orders", FamilyHygiene, ClassAnalyze},
		{"broken handoff key", "analyze_table", "ANALYZE public.orders", "autonomy:x", "",
			""},
		{"no action type", "", "ANALYZE public.orders", "", "", ""},
		{"cancel backend", "cancel_backend", "SELECT pg_cancel_backend(42)", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := PairForQueued(tc.actionType, tc.sql, tc.identity)
			if f != tc.family || (f != "" && c != tc.class) {
				t.Fatalf("pair = %s/%s, want %s/%s", f, c, tc.family, tc.class)
			}
		})
	}
}

func TestRegistryTrustViewOfAnUnboundDatabase(t *testing.T) {
	r := NewRegistry(true)
	if _, ok, err := r.TrustView(context.Background(), "nowhere"); ok || err != nil {
		t.Fatalf("unbound database: ok %v err %v", ok, err)
	}
	var nilRegistry *Registry
	if _, ok, err := nilRegistry.TrustView(context.Background(), "orders"); ok ||
		err != nil {
		t.Fatalf("nil registry: ok %v err %v", ok, err)
	}
}

func TestRegistryTrustViewIsAnnotated(t *testing.T) {
	lf := newLimiterFixture(t)
	r := NewRegistry(true)
	r.Register(lf.db, RegistryEntry{Service: lf.svc, Limiter: lf.lim})
	v, ok, err := r.TrustView(lf.ctx, lf.db)
	if err != nil || !ok || v.Database != lf.db || len(v.Rows) == 0 {
		t.Fatalf("view = %+v ok %v err %v", v, ok, err)
	}
	for _, row := range v.Rows {
		if row.Effective == nil {
			t.Fatalf("%s/%s has no effective level", row.Family, row.Class)
		}
	}
	r.Register("bare", RegistryEntry{Service: lf.svc})
	if v, ok, err = r.TrustView(lf.ctx, "bare"); err != nil || !ok ||
		v.Rows[0].Effective != nil {
		t.Fatalf("view without a limiter = %+v ok %v err %v", v.Rows[0], ok, err)
	}
}
