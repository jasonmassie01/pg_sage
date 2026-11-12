package clone

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// market-03: Destroy deletes only a sage-rh branch of the configured
// project, endpoint first, and treats a branch already gone (the server
// expiry) as done.

func TestLakebase_DestroyRefusesBranchesItDidNotName(t *testing.T) {
	for _, id := range []string{"", "projects/shop/branches/production",
		"projects/other/branches/sage-rh-0123abcd", "projects/shop/branches/sage-rh-0123ABCD",
		"projects/shop/branches/sage-rh-0123abc", "projects/shop/branches/sage-rh-0123abcd/x",
		"sage-rh-0123abcd"} {
		api := &fakeBranchAPI{}
		p := newLakebaseForTest(t, testLakebaseConfig(api))
		err := p.Destroy(context.Background(), Clone{ID: id})
		if !errors.Is(err, ErrInvalidLakebaseConfig) {
			t.Errorf("destroy %q = %v, want refused", id, err)
		}
		if calls := api.callList(); len(calls) != 0 {
			t.Errorf("destroy %q called the control plane: %q", id, calls)
		}
	}
}

func TestLakebase_DestroyOfAGoneBranchIsDone(t *testing.T) {
	gone := fmt.Errorf("branch: %w", ErrLakebaseNotFound)
	api := &fakeBranchAPI{deleteEndpointErr: gone, deleteBranchErr: gone}
	p := newLakebaseForTest(t, testLakebaseConfig(api))
	if err := p.Destroy(context.Background(),
		Clone{ID: "projects/shop/branches/sage-rh-0123abcd"}); err != nil {
		t.Fatalf("destroy of an expired branch = %v, want done", err)
	}
	if calls := api.callList(); len(calls) != 2 {
		t.Fatalf("calls = %q", calls)
	}
}

func TestLakebase_DestroyFailureNamesTheStep(t *testing.T) {
	cause := errors.New("service unavailable")
	api := &fakeBranchAPI{deleteBranchErr: cause}
	p := newLakebaseForTest(t, testLakebaseConfig(api))
	err := p.Destroy(context.Background(), Clone{ID: "projects/shop/branches/sage-rh-0123abcd"})
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "delete branch") {
		t.Fatalf("destroy failure = %v", err)
	}
	api = &fakeBranchAPI{deleteEndpointErr: cause}
	p = newLakebaseForTest(t, testLakebaseConfig(api))
	err = p.Destroy(context.Background(), Clone{ID: "projects/shop/branches/sage-rh-0123abcd"})
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "delete endpoint") {
		t.Fatalf("endpoint delete failure = %v", err)
	}
	if calls := api.callList(); len(calls) != 2 {
		t.Fatalf("calls = %q, want the branch delete still tried", calls)
	}
}

// Concurrent rehearsals each get their own branch.
func TestLakebase_ConcurrentCreatesGetDistinctBranches(t *testing.T) {
	api := &fakeBranchAPI{}
	p := newLakebaseForTest(t, testLakebaseConfig(api))
	const n = 8
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := p.Create(context.Background(), CloneSpec{})
			ids[i], errs[i] = c.ID, err
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, id := range ids {
		if errs[i] != nil || !testLakebaseID.MatchString(id) || seen[id] {
			t.Fatalf("create %d = %q, %v (seen before: %v)", i, id, errs[i], seen[id])
		}
		seen[id] = true
	}
}

func TestLakebase_SnapshotAgeIsZeroAtHead(t *testing.T) {
	p := newLakebaseForTest(t, testLakebaseConfig(&fakeBranchAPI{}))
	age, err := p.SnapshotAge(context.Background())
	if err != nil || age != 0 {
		t.Fatalf("snapshot age = %s, %v; a branch from head is current", age, err)
	}
}

func assertNoToken(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), testLakebaseToken) {
		t.Fatalf("error carries the minted token: %v", err)
	}
}
