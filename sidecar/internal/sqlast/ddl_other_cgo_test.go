//go:build cgo

package sqlast

import (
	"testing"
)

// Statements InspectDDL does not type (kind other) still carry what a
// reviewer needs to judge them: the tables TRUNCATE empties and LOCK
// locks (one part each, with the requested lock), the object a DROP of a
// schema, function, sequence or the like removes and its behavior, and
// the view a CREATE [OR REPLACE] VIEW defines. "Other" never means
// "nothing to see".

func TestInspectDDLTruncateNamesEveryTable(t *testing.T) {
	got, err := InspectDDL("TRUNCATE rv.rv_orders, rv_items CASCADE")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("TRUNCATE of two tables gave %d parts, want 2: %+v", len(got), got)
	}
	want := []QualifiedName{{Schema: "rv", Name: "rv_orders"}, {Name: "rv_items"}}
	for i, p := range got {
		if p.Kind != DDLOther || p.Command != "TruncateStmt" || p.Relation != want[i] ||
			p.Behavior != "cascade" || p.Part != i || p.Statement != 0 {
			t.Errorf("part %d = %+v, want other TruncateStmt on %v, cascade", i, p, want[i])
		}
	}
	plain, err := InspectDDL("TRUNCATE TABLE rv.rv_orders")
	if err != nil || len(plain) != 1 || plain[0].Behavior != "restrict" ||
		plain[0].Relation.Name != "rv_orders" {
		t.Fatalf("plain TRUNCATE = %+v, %v; want one restrict part on rv_orders", plain, err)
	}
}

func TestInspectDDLLockTableCarriesTheMode(t *testing.T) {
	cases := map[string]string{
		"LOCK TABLE rv.rv_orders IN ACCESS EXCLUSIVE MODE":   "ACCESS EXCLUSIVE",
		"LOCK rv.rv_orders":                                  "ACCESS EXCLUSIVE",
		"LOCK TABLE rv.rv_orders IN SHARE ROW EXCLUSIVE MODE": "SHARE ROW EXCLUSIVE",
		"LOCK TABLE rv.rv_orders IN ACCESS SHARE MODE":        "ACCESS SHARE",
		"LOCK TABLE rv.rv_orders IN SHARE MODE":               "SHARE",
		"LOCK TABLE rv.rv_orders IN EXCLUSIVE MODE":           "EXCLUSIVE",
		"LOCK TABLE rv.rv_orders IN ROW EXCLUSIVE MODE":       "ROW EXCLUSIVE",
		"LOCK TABLE rv.rv_orders IN SHARE UPDATE EXCLUSIVE MODE": "SHARE UPDATE " +
			"EXCLUSIVE",
	}
	for sql, mode := range cases {
		got, err := InspectDDL(sql)
		if err != nil || len(got) != 1 {
			t.Fatalf("InspectDDL(%q) = %+v, %v", sql, got, err)
		}
		p := got[0]
		if p.Kind != DDLOther || p.Command != "LockStmt" || p.TableLock != mode ||
			p.Relation != (QualifiedName{Schema: "rv", Name: "rv_orders"}) {
			t.Errorf("%q = %+v, want other LockStmt %s on rv.rv_orders", sql, p, mode)
		}
	}
	two, err := InspectDDL("LOCK TABLE a, b IN SHARE MODE")
	if err != nil || len(two) != 2 || two[1].Relation.Name != "b" || two[1].TableLock !=
		"SHARE" {
		t.Fatalf("LOCK of two tables = %+v, %v; want a part per table", two, err)
	}
}

func TestInspectDDLDropOfOtherObjects(t *testing.T) {
	cases := []struct {
		sql, objectType, behavior string
		object                    QualifiedName
		ifExists                  bool
	}{
		{"DROP SCHEMA rv CASCADE", "schema", "cascade", QualifiedName{Name: "rv"}, false},
		{"DROP SCHEMA IF EXISTS rv", "schema", "restrict", QualifiedName{Name: "rv"}, true},
		{"DROP FUNCTION rv.f(int)", "function", "restrict",
			QualifiedName{Schema: "rv", Name: "f"}, false},
		{"DROP SEQUENCE rv.s CASCADE", "sequence", "cascade",
			QualifiedName{Schema: "rv", Name: "s"}, false},
		{"DROP TYPE rv.mood", "type", "restrict", QualifiedName{Schema: "rv", Name: "mood"},
			false},
		{"DROP TRIGGER trg ON rv.rv_orders", "trigger", "restrict",
			QualifiedName{Name: "trg"}, false},
		{"DROP EXTENSION hypopg", "extension", "restrict", QualifiedName{Name: "hypopg"},
			false},
	}
	for _, c := range cases {
		got, err := InspectDDL(c.sql)
		if err != nil || len(got) != 1 {
			t.Fatalf("InspectDDL(%q) = %+v, %v", c.sql, got, err)
		}
		p := got[0]
		if p.Kind != DDLOther || p.Command != "DropStmt" || p.ObjectType != c.objectType ||
			p.Object != c.object || p.Behavior != c.behavior || p.IfExists != c.ifExists {
			t.Errorf("%q = %+v, want other DropStmt %s %v (%s, if exists %v)", c.sql, p,
				c.objectType, c.object, c.behavior, c.ifExists)
		}
	}
	schemas, err := InspectDDL("DROP SCHEMA a, b CASCADE")
	if err != nil || len(schemas) != 2 || schemas[1].Object.Name != "b" {
		t.Fatalf("DROP SCHEMA a, b = %+v, %v; want a part per schema", schemas, err)
	}
}

func TestInspectDDLViewNamesTheView(t *testing.T) {
	for _, sql := range []string{"CREATE VIEW rv.v AS SELECT 1",
		"CREATE OR REPLACE VIEW rv.v AS SELECT status FROM rv.rv_orders"} {
		got, err := InspectDDL(sql)
		if err != nil || len(got) != 1 {
			t.Fatalf("InspectDDL(%q) = %+v, %v", sql, got, err)
		}
		p := got[0]
		if p.Kind != DDLOther || p.Command != "ViewStmt" || p.ObjectType != "view" ||
			p.Relation != (QualifiedName{Schema: "rv", Name: "v"}) {
			t.Errorf("%q = %+v, want other ViewStmt view rv.v", sql, p)
		}
	}
}

// Statements that were already "other" keep their kind and command.
func TestInspectDDLOtherStatementsKeepTheirKind(t *testing.T) {
	for sql, command := range map[string]string{
		"TRUNCATE rv.rv_orders":           "TruncateStmt",
		"LOCK TABLE rv.rv_orders":         "LockStmt",
		"DROP SCHEMA rv":                  "DropStmt",
		"CREATE VIEW rv.v AS SELECT 1":    "ViewStmt",
		"ALTER INDEX rv.i RENAME TO j":    "RenameStmt",
		"CREATE TRIGGER trg AFTER INSERT ON rv.rv_orders FOR EACH ROW EXECUTE " +
			"FUNCTION rv.f()": "CreateTrigStmt",
	} {
		got, err := InspectDDL(sql)
		if err != nil || len(got) == 0 {
			t.Fatalf("InspectDDL(%q) = %+v, %v", sql, got, err)
		}
		if got[0].Kind != DDLOther || got[0].Command != command {
			t.Errorf("%q: kind %s command %s, want other %s", sql, got[0].Kind,
				got[0].Command, command)
		}
	}
}
