package executor

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/verify"
)

// No concurrent access tests: verificationClass is a pure function.

func TestVerificationClass(t *testing.T) {
	cases := map[string]string{
		"CREATE INDEX CONCURRENTLY idx ON public.t (a)":  verify.ClassIndexCreate,
		"create unique index concurrently u on t (a)":    verify.ClassIndexCreate,
		"DROP INDEX CONCURRENTLY public.idx":             verify.ClassIndexDrop,
		"  drop index concurrently if exists public.idx": verify.ClassIndexDrop,
		"ALTER SYSTEM SET work_mem = '64MB'":             verify.ClassGUC,
		"ALTER SYSTEM RESET random_page_cost":            verify.ClassGUC,
		"ALTER DATABASE app SET work_mem = '32MB'":       verify.ClassGUC,
		"ALTER TABLE public.t SET (autovacuum_vacuum_scale_factor = 0.02)": verify.
			ClassReloption,
		"ALTER TABLE public.t SET (fillfactor = 90)":                   verify.ClassReloption,
		"ALTER TABLE public.t RESET (fillfactor)":                      verify.ClassReloption,
		"VACUUM public.t":                                              verify.ClassVacuum,
		"VACUUM (ANALYZE) public.t":                                    verify.ClassVacuum,
		"ANALYZE public.t":                                             verify.ClassAnalyze,
		"INSERT INTO hint_plan.hints (query_id, hints) VALUES (1, '')": verify.ClassQueryHint,
		"DELETE FROM hint_plan.hints WHERE query_id = 1":               verify.ClassQueryHint,
		"DELETE FROM public.events WHERE created_at < now()":           verify.ClassRetention,
		"REINDEX INDEX CONCURRENTLY public.idx":                        verify.ClassReindex,
		"SELECT pg_cancel_backend(42)":                                 "",
		"":                                                             "",
	}
	for sql, want := range cases {
		if got := verificationClass(sql); got != want {
			t.Errorf("verificationClass(%q) = %q, want %q", sql, got, want)
		}
	}
}
