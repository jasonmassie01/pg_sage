package tuner

import "testing"

// pg_hint_plan syntax validation, shared by the deterministic rules and
// the tuning agent's hints (CheckHint).

func TestValidateHintSyntax_ValidDirectives(t *testing.T) {
	cases := []struct {
		name string
		hint string
		want bool
	}{
		{"HashJoin", "HashJoin(o c)", true},
		{"MergeJoin", "MergeJoin(a b)", true},
		{"NestLoop", "NestLoop(s)", true},
		{"IndexScan", "IndexScan(t idx_foo)", true},
		{"IndexOnlyScan", "IndexOnlyScan(t idx_bar)", true},
		{"SeqScan", "SeqScan(t)", true},
		{"NoSeqScan", "NoSeqScan(t)", true},
		{"Parallel", "Parallel(t 4)", true},
		{"NoParallel", "NoParallel(t)", true},
		{"BitmapScan", "BitmapScan(t idx_baz)", true},
		{"Set", `Set(work_mem "256MB")`, true},
		{"Combined", `Set(work_mem "128MB") HashJoin(o c)`, true},
		{"empty", "", false},
		{"spaces", "   ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validateHintSyntax(tc.hint)
			if got != tc.want {
				t.Errorf("validateHintSyntax(%q) = %v, want %v",
					tc.hint, got, tc.want)
			}
		})
	}
}

func TestValidateHintSyntax_RejectsSQL(t *testing.T) {
	cases := []string{
		"DROP TABLE users",
		"DELETE FROM hint_plan.hints",
		"INSERT INTO foo VALUES(1)",
		"ALTER TABLE foo ADD COLUMN bar int",
		"CREATE INDEX idx ON foo(bar)",
		"HashJoin(t); DROP TABLE users",
		"HashJoin(t) -- comment",
		"TRUNCATE hint_plan.hints",
		"GRANT ALL ON hint_plan.hints TO public",
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			if validateHintSyntax(tc) {
				t.Errorf("expected rejection for %q", tc)
			}
		})
	}
}

func TestSplitHintDirectives(t *testing.T) {
	parts := splitHintDirectives(
		`Set(work_mem "256MB") HashJoin(o c) IndexScan(t idx)`,
	)
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts, got %d: %v", len(parts), parts)
	}
	if parts[0] != `Set(work_mem "256MB")` {
		t.Errorf("part 0 = %q", parts[0])
	}
	if parts[1] != " HashJoin(o c)" {
		// Note: leading space is trimmed in validation
		got := parts[1]
		if got != " HashJoin(o c)" {
			t.Errorf("part 1 = %q", got)
		}
	}
}
