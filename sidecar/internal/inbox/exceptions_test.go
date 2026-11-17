package inbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// fleetpol-01c: every policy-set exception in force is a policy_exception
// item. One without an expiry holds until someone replaces it, so it is
// moderate; one with an expiry is safe and due when it reverts.
// No concurrent access tests: exceptionItem and joinSources hold no state.

func TestExceptionItem_NoExpiryIsModerateAndSaysSo(t *testing.T) {
	setAt := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	it := exceptionItem("billing", policy.Exception{DatabaseID: 9, SetName: "prod",
		SetBy: "alice@example.com", Reason: "month-end batch", SetAt: setAt,
		Diff: []policy.FieldDiff{{Field: "lock_duration_ceiling_ms"}}})
	if it.Kind != KindPolicyException || it.ID != "exception-9" || it.Database != "billing" {
		t.Fatalf("identity %+v", it)
	}
	if it.Risk != RiskModerate || it.DueAt != nil || !it.CreatedAt.Equal(setAt) {
		t.Fatalf("risk %s due %v created %v, want moderate, none, %v", it.Risk, it.DueAt,
			it.CreatedAt, setAt)
	}
	for _, want := range []string{"prod", "alice@example.com", "lock_duration_ceiling_ms"} {
		if !strings.Contains(it.Title, want) {
			t.Errorf("title %q lacks %q", it.Title, want)
		}
	}
	if !strings.Contains(it.Why, "no expiry") {
		t.Errorf("why %q does not say it never expires", it.Why)
	}
	if it.Href != "#/settings?tab=policy-sets&database=billing" ||
		it.Source != "sage.policy_exceptions" {
		t.Fatalf("href %q source %q", it.Href, it.Source)
	}
}

func TestExceptionItem_ExpiryIsSafeAndDue(t *testing.T) {
	expires := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	it := exceptionItem("billing", policy.Exception{DatabaseID: 9, SetName: "prod",
		SetBy: "alice@example.com", ExpiresAt: &expires})
	if it.Risk != RiskSafe || it.DueAt == nil || !it.DueAt.Equal(expires) {
		t.Fatalf("risk %s due %v, want safe due %v", it.Risk, it.DueAt, expires)
	}
	if !strings.Contains(it.Why, "2026-12-01") {
		t.Errorf("why %q lacks the expiry day", it.Why)
	}
	if !strings.Contains(it.Title, "same document as the set") {
		t.Errorf("an empty diff should say so: %q", it.Title)
	}
}

func TestExceptionItem_ManyFieldsAreShortened(t *testing.T) {
	diff := []policy.FieldDiff{{Field: "a"}, {Field: "b"}, {Field: "c"}, {Field: "d"}}
	it := exceptionItem("x", policy.Exception{DatabaseID: 1, SetName: "default",
		SetBy: "bob", Diff: diff})
	if !strings.Contains(it.Title, "a, b and 2 more") {
		t.Fatalf("title %q", it.Title)
	}
}

func TestJoinSources_ListsEveryPartAndFailsWhole(t *testing.T) {
	one := SourceFunc(func(context.Context, string) ([]Item, error) {
		return []Item{{ID: "1"}}, nil
	})
	two := SourceFunc(func(context.Context, string) ([]Item, error) {
		return []Item{{ID: "2"}, {ID: "3"}}, nil
	})
	got, err := joinSources(one, two).List(context.Background(), "orders")
	if err != nil || len(got) != 3 || got[0].ID != "1" || got[2].ID != "3" {
		t.Fatalf("got %+v, %v", got, err)
	}
	empty, err := joinSources().List(context.Background(), "orders")
	if err != nil || len(empty) != 0 {
		t.Fatalf("no parts: %+v, %v", empty, err)
	}
	boom := errors.New("read policy exceptions: connection refused")
	broken := SourceFunc(func(context.Context, string) ([]Item, error) { return nil, boom })
	got, err = joinSources(one, broken).List(context.Background(), "orders")
	if !errors.Is(err, boom) || got != nil {
		t.Fatalf("a failed part must fail the source (lower bound): %+v, %v", got, err)
	}
}

func TestPolicyExceptions_UnknownDatabaseIsNotConnected(t *testing.T) {
	_, err := PolicyExceptions(nil).List(context.Background(), "orders")
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
}
