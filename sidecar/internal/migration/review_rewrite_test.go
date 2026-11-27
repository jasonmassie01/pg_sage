package migration

import (
	"strings"
	"testing"
)

// No concurrent access tests: the rewrites are pure functions of the text.

// Each rule rewrite is deterministic: the same statement always gives the
// same guarded form.
func TestGuardedRewriteForms(t *testing.T) {
	cases := []struct{ sql, want string }{
		{"CREATE UNIQUE INDEX u ON app.t (c)", "CREATE UNIQUE INDEX CONCURRENTLY u ON app.t (c);"},
		{"ALTER TABLE app.o ADD CONSTRAINT fk FOREIGN KEY (c) REFERENCES app.c (id)",
			"ALTER TABLE app.o ADD CONSTRAINT fk FOREIGN KEY (c) REFERENCES app.c (id) " +
				"NOT VALID;\nALTER TABLE app.o VALIDATE CONSTRAINT fk;"},
		{"ALTER TABLE app.t ADD CONSTRAINT ck CHECK (v > 0)",
			"ALTER TABLE app.t ADD CONSTRAINT ck CHECK (v > 0) NOT VALID;\n" +
				"ALTER TABLE app.t VALIDATE CONSTRAINT ck;"},
		{"ALTER TABLE app.t ADD COLUMN id uuid DEFAULT gen_random_uuid()",
			"ALTER TABLE app.t ADD COLUMN id uuid;\n" +
				"ALTER TABLE app.t ALTER COLUMN id SET DEFAULT gen_random_uuid();"},
	}
	for _, tc := range cases {
		rep := mustReview(t, nil, tc.sql, ReviewOptions{PGVersion: 17})
		got := rep.Statements[0].GuardedRewrite
		if !strings.Contains(got, tc.want) {
			t.Errorf("%q: guarded rewrite %q, want it to contain %q", tc.sql, got, tc.want)
		}
		again := mustReview(t, nil, tc.sql, ReviewOptions{PGVersion: 17})
		if again.Statements[0].GuardedRewrite != got {
			t.Errorf("%q: rewrite is not deterministic", tc.sql)
		}
	}
}

// Invalid input to the rewrite: a statement with a string literal (which
// lint redacts) gets no rewrite and says why; an unnamed constraint keeps
// only the lock_timeout guard.
func TestGuardedRewriteRefusesRedactedText(t *testing.T) {
	rep := mustReview(t, nil, "ALTER TABLE app.t ADD CONSTRAINT ck CHECK (s <> 'x')",
		ReviewOptions{})
	st := rep.Statements[0]
	if st.GuardedRewrite != "" || !strings.Contains(st.Note, "string literal") {
		t.Fatalf("statement %+v, want no rewrite and a note", st)
	}
	rep = mustReview(t, nil, "ALTER TABLE app.t ADD CHECK (v > 0)", ReviewOptions{})
	st = rep.Statements[0]
	if strings.Contains(st.GuardedRewrite, "VALIDATE") {
		t.Fatalf("unnamed constraint rewritten: %q", st.GuardedRewrite)
	}
	if st.LockTimeoutMissing && !strings.HasPrefix(st.GuardedRewrite, guardLockTimeout) {
		t.Fatalf("lock_timeout guard missing: %q", st.GuardedRewrite)
	}
}

// A column definition the split cannot carry over (a constraint) is not
// split.
func TestGuardedRewriteKeepsComplexAddColumn(t *testing.T) {
	got := splitAddColumn("ALTER TABLE app.t ADD COLUMN id uuid DEFAULT gen_random_uuid() " +
		"NOT NULL")
	if got != "" {
		t.Fatalf("split a NOT NULL column: %q", got)
	}
	// ddl_drop_index_not_concurrent and the detach rule come from the
	// parse-tree classifier (cgo), so their rewrites are pinned directly.
	if got := ruleRewrite("ddl_drop_index_not_concurrent", "DROP INDEX app.i"); got !=
		"DROP INDEX CONCURRENTLY app.i;\n-- CONCURRENTLY cannot run inside a transaction block." {
		t.Fatalf("drop index rewrite %q", got)
	}
	if got := ruleRewrite("ddl_detach_partition_not_concurrent",
		"ALTER TABLE app.p DETACH PARTITION app.p1"); !strings.HasPrefix(got,
		"ALTER TABLE app.p DETACH PARTITION app.p1 CONCURRENTLY;") {
		t.Fatalf("detach rewrite %q", got)
	}
	if got := ruleRewrite("ddl_index_not_concurrent", "SELECT 1"); got != "" {
		t.Fatalf("rewrote a statement of the wrong shape: %q", got)
	}
}
