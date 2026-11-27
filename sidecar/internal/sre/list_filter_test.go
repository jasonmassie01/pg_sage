package sre

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// sre-09: the Cases page asks for the newest investigation of each case
// it shows, by id. ListFilter.Check bounds that request before any SQL.
// No concurrent access tests: Check is a pure function of its value.

func caseIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("case:%d", i)
	}
	return ids
}

func TestListFilter_CheckAcceptsCaseIDsUpToTheCap(t *testing.T) {
	for _, n := range []int{1, 2, MaxListCaseIDs} {
		if err := (ListFilter{CaseIDs: caseIDs(n)}).Check(); err != nil {
			t.Fatalf("%d case ids: %v, want accepted", n, err)
		}
	}
	long := strings.Repeat("x", 256)
	if err := (ListFilter{CaseIDs: []string{long, "b"}}).Check(); err != nil {
		t.Fatalf("a 256-character case id: %v, want accepted", err)
	}
	if err := (ListFilter{}).Check(); err != nil {
		t.Fatalf("an empty filter: %v, want accepted", err)
	}
	if err := (ListFilter{CaseID: "a", Cursor: "c"}).Check(); err != nil {
		t.Fatalf("single-case paging: %v, want accepted (contract unchanged)", err)
	}
}

func TestListFilter_CheckRejectsBadCaseIDs(t *testing.T) {
	cases := map[string]ListFilter{
		"101 ids":            {CaseIDs: caseIDs(MaxListCaseIDs + 1)},
		"257-character id":   {CaseIDs: []string{strings.Repeat("x", 257), "b"}},
		"empty id":           {CaseIDs: []string{"a", ""}},
		"control character":  {CaseIDs: []string{"a", "b\x00c"}},
		"with a cursor":      {CaseIDs: []string{"a", "b"}, Cursor: "c"},
		"with a single case": {CaseIDs: []string{"a", "b"}, CaseID: "a"},
	}
	for name, f := range cases {
		err := f.Check()
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
			continue
		}
		if !strings.Contains(err.Error(), "case") {
			t.Errorf("%s: error %q does not name the case ids", name, err)
		}
	}
}

func TestListFilter_UniqueCaseIDsKeepsFirstOrder(t *testing.T) {
	got := uniqueCaseIDs([]string{"b", "a", "b", "c", "a"})
	want := []string{"b", "a", "c"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("uniqueCaseIDs = %v, want %v", got, want)
	}
	if got := uniqueCaseIDs(nil); len(got) != 0 {
		t.Fatalf("uniqueCaseIDs(nil) = %v, want empty", got)
	}
}
