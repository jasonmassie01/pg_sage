package ledger

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// policy-07: the open-withheld filter is validated before any SQL runs,
// and the query it builds is parameterized. Database behaviour is in
// list_withheld_db_test.go.
//
// No concurrent access tests here: Validate and withheldQuery are pure.

func TestWithheldFilterValidate(t *testing.T) {
	valid := WithheldFilter{Limit: DefaultWithheldLimit}
	if err := valid.Validate(); err != nil {
		t.Fatalf("default filter: %v", err)
	}
	cases := []struct {
		name   string
		filter WithheldFilter
		field  string
	}{
		{"unknown verdict", WithheldFilter{Verdict: "foo", Limit: 1}, "verdict"},
		{"execute is not withheld", WithheldFilter{Verdict: VerdictExecute, Limit: 1}, "verdict"},
		{"zero limit", WithheldFilter{}, "limit"},
		{"negative limit", WithheldFilter{Limit: -1}, "limit"},
		{"limit past the cap", WithheldFilter{Limit: MaxWithheldLimit + 1}, "limit"},
	}
	for _, tc := range cases {
		err := tc.filter.Validate()
		if !errors.Is(err, ErrInvalidFilter) {
			t.Errorf("%s: err = %v, want ErrInvalidFilter", tc.name, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: %q does not name %s", tc.name, err, tc.field)
		}
	}
}

func TestWithheldFilterBoundaries(t *testing.T) {
	for _, limit := range []int{1, MaxWithheldLimit} {
		if err := (WithheldFilter{Limit: limit}).Validate(); err != nil {
			t.Errorf("limit %d: %v", limit, err)
		}
	}
	for _, verdict := range []Verdict{VerdictQueueApproval, VerdictPark, VerdictBlocked,
		VerdictObserveOnly} {
		if err := (WithheldFilter{Verdict: verdict, Limit: 1}).Validate(); err != nil {
			t.Errorf("verdict %s: %v", verdict, err)
		}
	}
	if DefaultWithheldLimit != 100 || MaxWithheldLimit != 500 {
		t.Fatalf("limits = %d/%d, want 100/500", DefaultWithheldLimit, MaxWithheldLimit)
	}
}

// Every filter value travels as a parameter; none is spliced into the SQL.
func TestWithheldQueryIsParameterized(t *testing.T) {
	since := time.Date(2026, 11, 1, 18, 0, 0, 0, time.UTC)
	databaseID := int64(42)
	sql, args := withheldQuery(WithheldFilter{DatabaseID: &databaseID,
		Verdict: VerdictPark, Reason: "x'; DROP TABLE sage.decision; --",
		Since: &since, Limit: 7})
	if strings.Contains(sql, "DROP TABLE") || strings.Contains(sql, "42") {
		t.Fatalf("filter values were spliced into the SQL:\n%s", sql)
	}
	if len(args) != 5 {
		t.Fatalf("args = %v, want database, verdict, reason, since, limit+1", args)
	}
	if got, ok := args[0].(*int64); !ok || *got != 42 {
		t.Errorf("database arg = %#v, want 42", args[0])
	}
	if args[1] != "parked" || args[2] != "x'; DROP TABLE sage.decision; --" {
		t.Errorf("verdict/reason args = %v/%v", args[1], args[2])
	}
	if got, ok := args[3].(*time.Time); !ok || !got.Equal(since) {
		t.Errorf("since arg = %#v, want %s", args[3], since)
	}
	if args[4] != 8 {
		t.Errorf("limit arg = %v, want 8 (one past the limit, to report truncation)", args[4])
	}
	for _, want := range []string{"resolved_at IS NULL", "verdict <> 'execute'",
		"candidate_key", "fingerprint"} {
		if !strings.Contains(sql, want) {
			t.Errorf("query lacks %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "proposed_sql") {
		t.Errorf("query reads the proposed SQL, which the list never returns:\n%s", sql)
	}
}

// Nil/empty: an empty filter has no database, verdict, reason or since.
func TestWithheldQueryEmptyFilter(t *testing.T) {
	_, args := withheldQuery(WithheldFilter{Limit: 1})
	if args[0].(*int64) != nil || args[1] != "" || args[2] != "" ||
		args[3].(*time.Time) != nil || args[4] != 2 {
		t.Fatalf("args = %#v, want nil database, empty verdict/reason, nil since, 2", args)
	}
}

func TestListOpenWithheldRejectsAnInvalidFilterBeforeSQL(t *testing.T) {
	entries, truncated, err := ListOpenWithheld(t.Context(), nil, WithheldFilter{Limit: 0})
	if !errors.Is(err, ErrInvalidFilter) || entries != nil || truncated {
		t.Fatalf("ListOpenWithheld = %v, %v, %v; want ErrInvalidFilter and nothing", entries,
			truncated, err)
	}
	_, _, err = ListOpenWithheld(t.Context(), nil, WithheldFilter{Limit: 1})
	if !errors.Is(err, ErrRepositoryUnavailable) {
		t.Fatalf("nil querier: err = %v, want ErrRepositoryUnavailable", err)
	}
}
