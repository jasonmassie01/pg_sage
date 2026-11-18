package config

import (
	"strings"
	"testing"
)

// ux-07a: trust.ask_after_approvals is how many person approvals of one
// class, with no rollback, make pg_sage ask to run it unattended. The
// default is 10 with no file and with a partial trust section; 0 turns
// the ask off (it is the one value that means "off", and it is kept).

// No concurrent access tests: loading and validating a config is
// single-threaded.

func TestAskAfterApprovals_DefaultsToTen(t *testing.T) {
	if got := DefaultConfig().Trust.AskAfterApprovals; got != 10 {
		t.Fatalf("DefaultConfig = %d, want 10", got)
	}
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Trust.AskAfterApprovals != 10 {
		t.Fatalf("no config file = %d, want 10", cfg.Trust.AskAfterApprovals)
	}
	cfg, err = loadRCAYAML(t, "trust:\n  level: advisory\n")
	if err != nil || cfg.Trust.AskAfterApprovals != 10 {
		t.Fatalf("partial trust section = %v (%v), want 10", cfg.Trust.AskAfterApprovals,
			err)
	}
}

func TestAskAfterApprovals_Boundaries(t *testing.T) {
	cases := []struct {
		yaml    string
		want    int
		wantErr bool
	}{
		{"trust:\n  ask_after_approvals: 0\n", 0, false},
		{"trust:\n  ask_after_approvals: 1\n", 1, false},
		{"trust:\n  ask_after_approvals: 25\n", 25, false},
		{"trust:\n  ask_after_approvals: 1000\n", 1000, false},
		{"trust:\n  ask_after_approvals: 1001\n", 0, true},
		{"trust:\n  ask_after_approvals: -1\n", 0, true},
	}
	for _, tc := range cases {
		cfg, err := loadRCAYAML(t, tc.yaml)
		switch {
		case tc.wantErr && (err == nil ||
			!strings.Contains(err.Error(), "trust.ask_after_approvals")):
			t.Errorf("%q: err %v, want one naming trust.ask_after_approvals", tc.yaml, err)
		case !tc.wantErr && err != nil:
			t.Errorf("%q: %v", tc.yaml, err)
		case !tc.wantErr && cfg.Trust.AskAfterApprovals != tc.want:
			t.Errorf("%q: %d, want %d", tc.yaml, cfg.Trust.AskAfterApprovals, tc.want)
		}
	}
}
