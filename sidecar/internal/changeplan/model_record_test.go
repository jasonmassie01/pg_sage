package changeplan

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// No concurrent access tests: checkModelPass is a pure function of its
// argument.

// Invalid input is refused before the store touches the database: the
// store here has no pool, so any query would panic.
func TestRecordModelPass_RefusesBadInputBeforeTheDatabase(t *testing.T) {
	s := NewStore(nil)
	cases := []struct {
		name string
		m    ModelPass
		want error
		says string
	}{
		{"over 1 MiB", ModelPass{Model: "m",
			RawOutput: strings.Repeat("x", MaxRawOutputBytes+1)}, ErrRawOutputTooLarge,
			"1048577 bytes"},
		{"no model", ModelPass{RawOutput: "x"}, ErrInvalidModelPass, "model"},
		{"long model", ModelPass{Model: strings.Repeat("m", 201), RawOutput: "x"},
			ErrInvalidModelPass, "model"},
		{"long prompt version", ModelPass{Model: "m",
			PromptVersion: strings.Repeat("p", 101)}, ErrInvalidModelPass, "prompt version"},
		{"bad pack hash", ModelPass{Model: "m", PackHash: "ABCD"}, ErrInvalidModelPass,
			"pack hash"},
		{"invalid utf-8", ModelPass{Model: "m", RawOutput: "ok \xff\xfe"},
			ErrInvalidModelPass, "UTF-8"},
		{"nul byte", ModelPass{Model: "m", RawOutput: "a\x00b"}, ErrInvalidModelPass,
			"NUL"},
	}
	for _, tc := range cases {
		_, err := s.RecordModelPass(context.Background(), 1, tc.m)
		if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: %v, want %v saying %q", tc.name, err, tc.want, tc.says)
		}
	}
	if _, err := s.RecordModelPass(context.Background(), 0, ModelPass{Model: "m"}); err == nil ||
		!strings.Contains(err.Error(), "plan") {
		t.Errorf("plan 0: %v, want a refusal naming the plan", err)
	}
}

// Boundary: exactly 1 MiB, an empty output and markdown fences pass the
// checks, and the hash is the SHA-256 of the exact bytes.
func TestCheckModelPass_AcceptsTheLimitAndHashesExactBytes(t *testing.T) {
	for _, raw := range []string{"", fenced, strings.Repeat("x", MaxRawOutputBytes)} {
		m := ModelPass{Model: "m", RawOutput: raw}
		if err := checkModelPass(m); err != nil {
			t.Errorf("%d bytes: %v", len(raw), err)
		}
		if got := rawOutputSHA256(raw); got != sha256Hex(raw) {
			t.Errorf("%d bytes hash %s", len(raw), got)
		}
	}
	if rawOutputSHA256("") != sha256Empty {
		t.Fatal("the empty output does not hash to SHA-256 of the empty string")
	}
}
