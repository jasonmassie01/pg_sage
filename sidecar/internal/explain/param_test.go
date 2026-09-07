package explain

import (
	"testing"
)

// ---------------------------------------------------------------------------
// countParamPlaceholders
// ---------------------------------------------------------------------------

func TestCountParamPlaceholders_SingleParam(t *testing.T) {
	got := countParamPlaceholders("SELECT * FROM t WHERE id = $1")
	if got != 1 {
		t.Errorf("countParamPlaceholders($1) = %d, want 1", got)
	}
}

func TestCountParamPlaceholders_TwoParams(t *testing.T) {
	got := countParamPlaceholders(
		"SELECT * FROM t WHERE a = $1 AND b = $2")
	if got != 2 {
		t.Errorf("countParamPlaceholders($1,$2) = %d, want 2", got)
	}
}

func TestCountParamPlaceholders_HighestWins(t *testing.T) {
	// $3 is the highest even though $1 and $2 are absent.
	got := countParamPlaceholders("SELECT * FROM t WHERE a = $3")
	if got != 3 {
		t.Errorf("countParamPlaceholders($3 only) = %d, want 3", got)
	}
}

func TestCountParamPlaceholders_NoParams(t *testing.T) {
	got := countParamPlaceholders("SELECT * FROM t")
	if got != 0 {
		t.Errorf("countParamPlaceholders(no params) = %d, want 0", got)
	}
}

func TestCountParamPlaceholders_EmptyString(t *testing.T) {
	got := countParamPlaceholders("")
	if got != 0 {
		t.Errorf("countParamPlaceholders(\"\") = %d, want 0", got)
	}
}

func TestCountParamPlaceholders_RepeatedParam(t *testing.T) {
	// $1 appears three times but highest is still 1.
	got := countParamPlaceholders("SELECT $1, $1, $1")
	if got != 1 {
		t.Errorf("countParamPlaceholders(repeated $1) = %d, want 1",
			got)
	}
}

func TestCountParamPlaceholders_DoubleDigit(t *testing.T) {
	got := countParamPlaceholders("SELECT $10, $2")
	if got != 10 {
		t.Errorf("countParamPlaceholders($10,$2) = %d, want 10", got)
	}
}

// ---------------------------------------------------------------------------
// hasParamPlaceholder — additional targeted cases
// ---------------------------------------------------------------------------

func TestHasParamPlaceholder_WithParam(t *testing.T) {
	got := hasParamPlaceholder("SELECT $1")
	if !got {
		t.Error("hasParamPlaceholder(\"SELECT $1\") = false, want true")
	}
}

func TestHasParamPlaceholder_NoParam(t *testing.T) {
	got := hasParamPlaceholder("SELECT * FROM t")
	if got {
		t.Error(
			"hasParamPlaceholder(\"SELECT * FROM t\") = true, want false")
	}
}

func TestHasParamPlaceholder_DoubleDollar(t *testing.T) {
	// $$ is used for PL/pgSQL blocks, not numbered placeholders.
	// The regex \$\d matches $$ only if the second $ is followed by
	// a digit, which it is not. "SELECT $$" should be false.
	got := hasParamPlaceholder("SELECT $$")
	if got {
		t.Error(
			"hasParamPlaceholder(\"SELECT $$\") = true, want false")
	}
}
