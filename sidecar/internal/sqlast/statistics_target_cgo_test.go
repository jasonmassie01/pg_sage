//go:build cgo

package sqlast

import (
	"errors"
	"testing"
)

// review-02c: one ALTER COLUMN ... SET STATISTICS clause with a target the
// executor sets passes the parse-tree layer; everything else is refused.
func TestCheckSetStatisticsTarget(t *testing.T) {
	allowed := []string{
		`ALTER TABLE public.t ALTER COLUMN c SET STATISTICS 1000`,
		`ALTER TABLE public.t ALTER c SET STATISTICS 1`,
		`ALTER TABLE public.t ALTER COLUMN "we""ird;col" SET STATISTICS 10000`,
		`ALTER TABLE public.t ALTER COLUMN c SET STATISTICS -1`,
	}
	for _, sql := range allowed {
		if err := Check(sql, testRules); err != nil {
			t.Errorf("Check(%q) = %v, want allowed", sql, err)
		}
	}
	refused := []string{
		`ALTER TABLE public.t ALTER COLUMN c SET STATISTICS 0`,
		`ALTER TABLE public.t ALTER COLUMN c SET STATISTICS 10001`,
		`ALTER TABLE public.t ALTER COLUMN c SET STATISTICS -2`,
		`ALTER TABLE public.t ALTER COLUMN c SET STATISTICS DEFAULT`,
		`ALTER TABLE public.t ALTER COLUMN c SET STATISTICS 100, DROP COLUMN x`,
		`ALTER TABLE t ALTER COLUMN c SET STATISTICS 100`,
		`ALTER INDEX public.i ALTER COLUMN 1 SET STATISTICS 100`,
	}
	for _, sql := range refused {
		if err := Check(sql, testRules); !errors.Is(err, ErrRejected) {
			t.Errorf("Check(%q) = %v, want rejection", sql, err)
		}
	}
}
