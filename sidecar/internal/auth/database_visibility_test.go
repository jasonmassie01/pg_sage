package auth

import (
	"fmt"
	"sync"
	"testing"
)

// apiauth-08b: VisibleDatabases is the read side of the grant rules. A
// user with no grants, and every admin, is unscoped (nil set, no filter);
// a user with any grant sees exactly the granted databases, whatever the
// grant's role. An unknown global role fails closed: scoped to nothing.
// No database test here: the function is pure (the store is tested in
// database_grants_db_test.go).

func TestVisibleDatabases(t *testing.T) {
	cases := []struct {
		name       string
		global     string
		grants     map[string]string
		wantScoped bool
		want       []string
	}{
		{"viewer without grants is unscoped", RoleViewer, nil, false, nil},
		{"operator with an empty map is unscoped", RoleOperator, map[string]string{}, false,
			nil},
		{"admin with grants is never scoped", RoleAdmin,
			map[string]string{"billing": RoleViewer}, false, nil},
		{"viewer grant", RoleViewer, map[string]string{"billing": RoleViewer}, true,
			[]string{"billing"}},
		{"operator grants of both roles", RoleOperator,
			map[string]string{"billing": RoleOperator, "search": RoleViewer}, true,
			[]string{"billing", "search"}},
		{"unknown global role fails closed", "superuser",
			map[string]string{"billing": RoleViewer}, true, nil},
		{"empty global role fails closed", "", nil, true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set, scoped := VisibleDatabases(tc.global, tc.grants)
			if scoped != tc.wantScoped {
				t.Fatalf("scoped = %v, want %v", scoped, tc.wantScoped)
			}
			if !scoped {
				if set != nil {
					t.Fatalf("unscoped set = %v, want nil", set)
				}
				return
			}
			if set == nil {
				t.Fatal("a scoped user must get a non-nil set (empty means nothing)")
			}
			if len(set) != len(tc.want) {
				t.Fatalf("set = %v, want %v", set, tc.want)
			}
			for _, db := range tc.want {
				if !set[db] {
					t.Fatalf("set = %v, missing %q", set, db)
				}
			}
		})
	}
}

func TestVisibleDatabases_IgnoresInvalidGrantNames(t *testing.T) {
	// "all" and "*" can never be stored (validGrantDatabase), but a read
	// must not turn them into "every database" if one ever appears.
	set, scoped := VisibleDatabases(RoleViewer,
		map[string]string{"all": RoleViewer, "*": RoleViewer, "": RoleViewer,
			"billing": RoleViewer})
	if !scoped || len(set) != 1 || !set["billing"] {
		t.Fatalf("set = %v scoped = %v, want only billing", set, scoped)
	}
}

func TestVisibleDatabases_DoesNotAliasGrants(t *testing.T) {
	grants := map[string]string{"billing": RoleViewer}
	set, _ := VisibleDatabases(RoleViewer, grants)
	set["search"] = true
	if _, ok := grants["search"]; ok {
		t.Fatal("the visible set shares storage with the grants map")
	}
}

func TestVisibleDatabases_Concurrent(t *testing.T) {
	grants := map[string]string{"billing": RoleViewer}
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			set, scoped := VisibleDatabases(RoleViewer, grants)
			if !scoped || len(set) != 1 || !set["billing"] {
				errs <- fmt.Errorf("set %v scoped %v", set, scoped)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
