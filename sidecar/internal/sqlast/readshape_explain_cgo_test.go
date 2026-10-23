//go:build cgo

package sqlast_test

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/explain"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// CHECK-SQL-05: every read shape is a single read statement the plan-only
// EXPLAIN path accepts. explain imports sqlast, so this lives in the
// external test package.
func TestReadShapePassesExplainReadStatement(t *testing.T) {
	for _, in := range []string{
		"UPDATE s.t SET a = 1 WHERE b = $1",
		"DELETE FROM rv.orders WHERE created_at < now()",
		"UPDATE ONLY rv.orders SET a = 1",
		"INSERT INTO rv.archive (id) SELECT id FROM rv.orders WHERE status = 'void'",
		"WITH v AS (SELECT id FROM rv.orders) INSERT INTO rv.archive (id) SELECT id FROM v",
		"INSERT INTO rv.t (a) VALUES (1), (2)",
		"SELECT id FROM rv.orders WHERE id = $1 FOR UPDATE",
		"WITH x AS (SELECT 1 AS a) UPDATE rv.t SET a = 2 WHERE a IN (SELECT a FROM x)",
		"UPDATE rv.t SET note = 'x; y' WHERE note = '--'",
	} {
		shape, err := sqlast.ReadShape(in)
		if err != nil {
			t.Errorf("ReadShape(%q) error = %v", in, err)
			continue
		}
		body, err := explain.ReadStatement(shape)
		if err != nil {
			t.Errorf("explain.ReadStatement(%q) error = %v", shape, err)
			continue
		}
		if body != shape {
			t.Errorf("ReadStatement trimmed %q to %q", shape, body)
		}
	}
}
