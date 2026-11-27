package sre

import "fmt"

// MaxListCaseIDs bounds the cases one latest-per-case list names: one
// index probe per id.
const MaxListCaseIDs = 100

// Check reports a filter the list cannot serve: CaseIDs beyond
// MaxListCaseIDs, a case id that is empty, longer than 256 characters or
// holds control characters, or CaseIDs together with Cursor or CaseID
// (the latest-per-case answer is one page, never a paged case history).
func (f ListFilter) Check() error {
	if len(f.CaseIDs) == 0 {
		return nil
	}
	switch {
	case len(f.CaseIDs) > MaxListCaseIDs:
		return fmt.Errorf("%w: %d case ids, at most %d per request", ErrInvalidRequest,
			len(f.CaseIDs), MaxListCaseIDs)
	case f.Cursor != "":
		return fmt.Errorf("%w: several case ids are one page; drop the cursor",
			ErrInvalidRequest)
	case f.CaseID != "":
		return fmt.Errorf("%w: name the case ids either once or as a list, not both",
			ErrInvalidRequest)
	}
	for _, id := range f.CaseIDs {
		if err := checkText("case id", id, true, 256); err != nil {
			return err
		}
	}
	return nil
}

// uniqueCaseIDs drops repeated ids, keeping the first of each.
func uniqueCaseIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
