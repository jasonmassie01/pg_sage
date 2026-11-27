package evidence

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// audit-07: a continuation pack names one window file and the id to page
// after. Every other combination is refused before any query runs.
func TestScopePagingValidation(t *testing.T) {
	now := time.Now()
	win := Scope{From: now.Add(-time.Hour), To: now}
	with := func(file string, after int64) Scope {
		s := win
		s.File, s.AfterID = file, after
		return s
	}
	bad := map[string]Scope{
		"after_id without file":  with("", 10),
		"negative after_id":      with("actions.json", -1),
		"unknown file":           with("nope.json", 0),
		"action-pack file":       with("action.json", 0),
		"manifest":               with("manifest.json", 0),
		"file on an action pack": {ActionID: 3, File: "actions.json"},
	}
	for name, s := range bad {
		if err := s.validate(); !errors.Is(err, ErrScope) {
			t.Errorf("%s: err = %v, want ErrScope", name, err)
		}
	}
	for _, file := range []string{"actions.json", "auth_audit.json", "config_audit.json",
		"pgaudit.json", "guard_query_audit.json", "decisions.json"} {
		for _, after := range []int64{0, 1, 1 << 62} {
			if err := with(file, after).validate(); err != nil {
				t.Errorf("%s after %d: %v", file, after, err)
			}
		}
	}
	if err := win.validate(); err != nil {
		t.Errorf("a plain window: %v", err)
	}
}

// Build refuses a missing audited database whatever the control source,
// and an invalid scope before touching either.
func TestBuildNeedsTheAuditedDatabase(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	_, err := Build(ctx, Sources{}, Scope{ActionID: 1}, nil, now)
	if err == nil || !strings.Contains(err.Error(), "database") {
		t.Fatalf("no database: err = %v", err)
	}
	if _, err := Build(ctx, Sources{}, Scope{}, nil, now); err == nil {
		t.Fatalf("an empty scope with no database was accepted")
	}
}
