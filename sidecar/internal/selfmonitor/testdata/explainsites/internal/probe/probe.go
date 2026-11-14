// Package probe is the explain-sites check's fixture (security-01c-n):
// parsed, never built.
package probe

func pinned(ctx context.Context, tx pgx.Tx) error {
	if err := apppath.Pin(ctx, tx); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "EXPLAIN (FORMAT JSON) SELECT 1")
	return err
}

func unpinned(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "EXPLAIN "+"SELECT 1")
	return err
}

func (s *session) open(ctx context.Context, tx pgx.Tx) error { return apppath.Pin(ctx, tx) }

func (s *session) plan(ctx context.Context, q string) error {
	_, err := s.tx.Exec(ctx, `EXPLAIN (GENERIC_PLAN) `+q)
	return err
}

func ownPath() string { return "SET LOCAL search_path = pg_catalog, app" }

func ownPlan() string { return "EXPLAIN\n(FORMAT JSON) SELECT 1" }

var keywords = []string{"DELETE", "EXPLAIN", "GRANT"}

const note = "Run EXPLAIN (ANALYZE) yourself"

const word = "EXPLAINED is not the keyword"
