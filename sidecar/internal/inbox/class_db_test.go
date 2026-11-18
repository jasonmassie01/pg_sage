package inbox

import (
	"context"
	"testing"
)

// A change approval names its action class (ux-07b), so the Inbox can
// offer "Select all <class> on <db>"; the other kinds name none.
// No concurrent test: one read of a seeded database.
func TestInboxDB_ChangeApprovalNamesItsClass(t *testing.T) {
	pool := inboxDB(t, "inbox_class")
	seedSix(t, pool, "orders")
	svc, _ := inboxFleet(t, pool, "orders")
	res := svc.List(context.Background(), Request{Databases: []string{"orders"}})
	got := byKind(res)
	if c := got[KindChangeApproval].Class; c != "create_index" {
		t.Fatalf("change approval class = %q, want create_index", c)
	}
	for k, it := range got {
		if k != KindChangeApproval && it.Class != "" {
			t.Errorf("%s names class %q; only change approvals have one", k, it.Class)
		}
	}
}
