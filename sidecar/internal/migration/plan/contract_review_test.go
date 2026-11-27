package plan

import (
	"strings"
	"testing"
)

// No concurrent access tests: ContractScript is a pure function of the
// plan (migration-06 review).

// evilPlan is a SET NOT NULL contract whose table name holds a newline
// and SQL: identifiers may contain any character, and the table name
// reaches the script from the agent's request.
func evilPlan(table string) Plan {
	return Plan{
		ContractSteps: []Step{{Kind: StepSetNotNull, Phase: PhaseContract,
			SQL: `ALTER TABLE "public"."x" ALTER COLUMN "c" SET NOT NULL`}},
		ContractGuard: ContractGuard{Kind: GuardValidatedCheck, Schema: "public",
			Table: table, Name: "x_c_nn"},
	}
}

// Invalid input: a control character in a name never ends the header
// comment, so nothing an operator runs precedes BEGIN.
func TestContractScriptHeaderSurvivesControlCharacters(t *testing.T) {
	for name, table := range map[string]string{
		"newline":         "x\nDROP TABLE public.users; --",
		"carriage return": "x\rDROP TABLE public.users; --",
		"crlf":            "x\r\nDROP TABLE public.users; --",
	} {
		t.Run(name, func(t *testing.T) {
			script, err := evilPlan(table).ContractScript()
			if err != nil {
				t.Fatalf("ContractScript: %v", err)
			}
			head, _, found := strings.Cut(script, "\nBEGIN;\n")
			if !found {
				t.Fatalf("no BEGIN line in %q", script)
			}
			for _, line := range strings.FieldsFunc(head, func(r rune) bool {
				return r == '\n' || r == '\r'
			}) {
				if !strings.HasPrefix(line, "--") {
					t.Fatalf("header line %q is not a comment:\n%s", line, script)
				}
			}
		})
	}
}

// A backslash in a quoted value is doubled in an escape-string (E) literal, so the
// guard means the same with standard_conforming_strings on or off.
func TestLiteralIsIndependentOfStandardConformingStrings(t *testing.T) {
	tests := map[string]string{
		"users_email_nn": `'users_email_nn'`,
		"it's":           `'it''s'`,
		`chk\x`:          `E'chk\\x'`,
		`a\'b`:           `E'a\\''b'`,
	}
	for in, want := range tests {
		if got := literal(in); got != want {
			t.Fatalf("literal(%q) = %s, want %s", in, got, want)
		}
	}
}
