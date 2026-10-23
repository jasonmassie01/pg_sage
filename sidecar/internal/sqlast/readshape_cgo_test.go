//go:build cgo

package sqlast

import (
	"errors"
	"testing"
)

// readShapeCases are shared with the explain.ReadStatement test in the
// external test package.
var readShapeCases = []struct{ in, want string }{
	{"UPDATE s.t SET a = 1 WHERE b = $1", "SELECT 1 FROM s.t WHERE b = $1"},
	{"DELETE FROM rv.orders WHERE created_at < now()",
		"SELECT 1 FROM rv.orders WHERE created_at < now()"},
	{"UPDATE rv.orders o SET status = 'x' WHERE o.created_at < $1 RETURNING id",
		"SELECT 1 FROM rv.orders o WHERE o.created_at < $1"},
	{"UPDATE ONLY rv.orders SET a = 1", "SELECT 1 FROM ONLY rv.orders"},
	{"DELETE FROM rv.orders", "SELECT 1 FROM rv.orders"},
	{"INSERT INTO rv.archive (id) SELECT id FROM rv.orders WHERE status = 'void'",
		"SELECT id FROM rv.orders WHERE status = 'void'"},
	{"WITH v AS (SELECT id FROM rv.orders) INSERT INTO rv.archive (id) SELECT id FROM v",
		"WITH v AS (SELECT id FROM rv.orders) SELECT id FROM v"},
	{"INSERT INTO rv.t (a) VALUES (1), (2)", "VALUES (1), (2)"},
	{"SELECT id FROM rv.orders WHERE id = $1 FOR UPDATE",
		"SELECT id FROM rv.orders WHERE id = $1"},
	{"WITH x AS (SELECT 1 AS a) UPDATE rv.t SET a = 2 WHERE a IN (SELECT a FROM x)",
		"WITH x AS (SELECT 1 AS a) SELECT 1 FROM rv.t WHERE a IN (SELECT a FROM x)"},
}

// CHECK-SQL-05: writes become the read that finds their rows.
func TestReadShape(t *testing.T) {
	for _, c := range readShapeCases {
		got, err := ReadShape(c.in)
		if err != nil {
			t.Errorf("ReadShape(%q) error = %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ReadShape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestReadShapeFingerprintEqualsHandWrittenSelect(t *testing.T) {
	shape, err := ReadShape("UPDATE s.t SET a = 1 WHERE b = $1")
	if err != nil {
		t.Fatalf("ReadShape error = %v", err)
	}
	if got, want := mustFingerprint(t, shape),
		mustFingerprint(t, "SELECT 1 FROM s.t WHERE b = $1"); got != want {
		t.Errorf("fingerprint of %q = %s, want %s", shape, got, want)
	}
	if mustFingerprint(t, shape) == mustFingerprint(t, "SELECT 1 FROM s.t WHERE c = $1") {
		t.Error("read shape fingerprint ignores the predicate column")
	}
}

func TestReadShapeUnsupported(t *testing.T) {
	for _, sql := range []string{
		"UPDATE rv.orders o SET status = s.status FROM rv.staging s WHERE s.id = o.id",
		"DELETE FROM rv.items i USING rv.orders o WHERE i.order_id = o.id",
		"MERGE INTO t USING s ON t.id = s.id WHEN MATCHED THEN DELETE",
		"INSERT INTO rv.t DEFAULT VALUES",
		"CREATE INDEX i ON rv.t (a)",
		"WITH d AS (DELETE FROM rv.t RETURNING id) SELECT id FROM d",
		"WITH d AS (DELETE FROM rv.t RETURNING id) " +
			"UPDATE rv.u SET a = 1 WHERE id IN (SELECT id FROM d)",
		"UPDATE rv.t SET a = 1 WHERE CURRENT OF c",
		"SELECT * INTO rv.copy FROM rv.t",
		"VACUUM rv.t",
	} {
		got, err := ReadShape(sql)
		if got != "" || !errors.Is(err, ErrUnsupported) || errors.Is(err, ErrRejected) {
			t.Errorf("ReadShape(%q) = %q, %v; want ErrUnsupported", sql, got, err)
		}
	}
}

func TestReadShapeRejected(t *testing.T) {
	for _, sql := range []string{"", "  ;", "DELETE FROM t; DELETE FROM u", "UPDATE t SET"} {
		got, err := ReadShape(sql)
		if got != "" || !errors.Is(err, ErrRejected) || errors.Is(err, ErrUnsupported) {
			t.Errorf("ReadShape(%q) = %q, %v; want ErrRejected", sql, got, err)
		}
	}
}
