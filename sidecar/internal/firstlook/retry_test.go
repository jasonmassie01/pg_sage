package firstlook

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestRetryableError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"statement timeout", &pgconn.PgError{Code: "57014"}, true},
		{"wrapped statement timeout", fmt.Errorf("read indexes: %w",
			&pgconn.PgError{Code: "57014"}), true},
		{"lock timeout", &pgconn.PgError{Code: "55P03"}, true},
		{"serialization failure", &pgconn.PgError{Code: "40001"}, true},
		{"deadlock", &pgconn.PgError{Code: "40P01"}, true},
		{"lost connection", errors.New("conn closed"), true},
		{"permission denied", &pgconn.PgError{Code: "42501"}, false},
		{"missing relation", &pgconn.PgError{Code: "42P01"}, false},
		{"feature not supported", &pgconn.PgError{Code: "0A000"}, false},
	} {
		if got := retryableError(tc.err); got != tc.want {
			t.Errorf("%s: retryable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Only pg_sage's own session SET is replaced by the first look's budget;
// every other non-zero source is an operator's limit, named in the note.
func TestOperatorLimit(t *testing.T) {
	for _, tc := range []struct {
		setting int
		source  string
		wantMS  int
		wantBy  string
	}{
		{500, "session", 0, ""},
		{0, "default", 0, ""},
		{0, "user", 0, ""},
		{-1, "user", 0, ""},
		{300, "user", 300, "role"},
		{300, "database", 300, "database"},
		{300, "database user", 300, "role in this database"},
		{300, "configuration file", 300, "server configuration"},
		{300, "command line", 300, "server configuration"},
		{300, "client", 300, "connection options"},
		{300, "some future source", 300, "some future source"},
	} {
		ms, by := operatorLimit(tc.setting, tc.source)
		if ms != tc.wantMS || !strings.Contains(by, tc.wantBy) || (tc.wantBy == "" && by != "") {
			t.Errorf("operatorLimit(%d, %q) = %d %q, want %d %q", tc.setting, tc.source, ms,
				by, tc.wantMS, tc.wantBy)
		}
	}
}

func TestDegradeReasonNamesWhoSetTheTimeout(t *testing.T) {
	err := &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement " +
		"timeout"}
	own := degradeReason(err, 5000, "")
	if !strings.Contains(own, "5000 ms") || strings.Contains(own, "set by") {
		t.Fatalf("own budget reason = %q", own)
	}
	by := degradeReason(err, 300, "safety.query_timeout_ms")
	if !strings.Contains(by, "300 ms") || !strings.Contains(by, "safety.query_timeout_ms") {
		t.Fatalf("operator reason = %q", by)
	}
}

func TestMergeRetry(t *testing.T) {
	prev := Report{ID: 7, Database: "app", Relations: 0, StatementTimeoutMS: 500,
		Items: []Item{{Rule: RuleXIDRunway, Severity: SeverityInfo, Object: "db"}},
		Checks: []Check{
			{Rule: RuleDuplicateIndex, Status: CheckDegraded, Note: "statement timeout"},
			{Rule: RuleXIDRunway, Status: CheckFinding, Note: "age 1"},
			{Rule: RuleMissingExtension, Status: CheckDegraded, Note: "conn closed"},
			{Rule: RuleSequenceRunway, Status: CheckDegraded, Note: "permission denied"},
		},
		Capabilities: []Capability{{Name: "old"}},
		Retryable:    []string{RuleDuplicateIndex, RuleMissingExtension},
	}
	fresh := Report{Relations: 42, StatementTimeoutMS: 5000,
		Items: []Item{{Rule: RuleDuplicateIndex, Severity: SeverityWarning, Object: "a.b"}},
		Checks: []Check{
			{Rule: RuleDuplicateIndex, Status: CheckFinding},
			{Rule: RuleMissingExtension, Status: CheckDegraded, Note: "statement timeout"},
		},
		Capabilities: []Capability{{Name: "new"}},
		Retryable:    []string{RuleMissingExtension},
	}
	got := mergeRetry(prev, fresh, []string{RuleDuplicateIndex, RuleMissingExtension})
	if got.ID != 7 || got.Relations != 42 || got.StatementTimeoutMS != 500 {
		t.Fatalf("header = id %d relations %d timeout %d", got.ID, got.Relations,
			got.StatementTimeoutMS)
	}
	if len(got.Items) != 2 || got.Items[0].Rule != RuleDuplicateIndex {
		t.Fatalf("items = %+v, want the new warning first, then the kept info", got.Items)
	}
	want := []Check{
		{Rule: RuleDuplicateIndex, Status: CheckFinding, Retried: true,
			Note: "first attempt: statement timeout"},
		{Rule: RuleXIDRunway, Status: CheckFinding, Note: "age 1"},
		{Rule: RuleMissingExtension, Status: CheckDegraded, Retried: true,
			Note: "statement timeout; first attempt: conn closed"},
		{Rule: RuleSequenceRunway, Status: CheckDegraded, Note: "permission denied"},
	}
	if len(got.Checks) != len(want) {
		t.Fatalf("checks = %+v", got.Checks)
	}
	for i := range want {
		if got.Checks[i] != want[i] {
			t.Fatalf("check %d = %+v, want %+v", i, got.Checks[i], want[i])
		}
	}
	if len(got.Capabilities) != 1 || got.Capabilities[0].Name != "new" {
		t.Fatalf("capabilities = %+v, want the retried extension step's", got.Capabilities)
	}
	if len(got.Retryable) != 0 {
		t.Fatalf("retryable = %v: a check is retried only once", got.Retryable)
	}
	if len(prev.Checks) != 4 || prev.Checks[0].Retried || len(prev.Items) != 1 {
		t.Fatalf("merge changed its input: %+v", prev)
	}
}

func TestMergeRetryKeepsWhatWasNotRerun(t *testing.T) {
	prev := Report{Relations: 9, Capabilities: []Capability{{Name: "kept"}},
		Checks: []Check{{Rule: RuleTableBloat, Status: CheckDegraded, Note: "timeout"}},
		Retryable: []string{RuleTableBloat}}
	got := mergeRetry(prev, Report{Relations: 0}, nil)
	if got.Relations != 9 || len(got.Capabilities) != 1 || got.Checks[0] != prev.Checks[0] {
		t.Fatalf("merge without a rerun = %+v", got)
	}
	if got.FactProposals != nil {
		t.Fatalf("fact proposals = %+v, want none: the test-schema step did not rerun",
			got.FactProposals)
	}
}
