package lakebaseid

import (
	"regexp"
	"strings"
	"testing"
)

// market-06b: the rehearsal branch prefix is shared by the clone provider
// (which creates sage-rh-<8 hex> branches) and fleet discovery (which skips
// them). It is a constant: no nil, concurrency or state cases apply.
func TestRehearsalBranchPrefix_IsAValidBranchIDStart(t *testing.T) {
	if RehearsalBranchPrefix != "sage-rh-" {
		t.Fatalf("RehearsalBranchPrefix = %q, want sage-rh-", RehearsalBranchPrefix)
	}
	id := regexp.MustCompile(`^` + Pattern + `$`)
	for _, branch := range []string{RehearsalBranchPrefix + "0a1b2c3d",
		RehearsalBranchPrefix + "ffffffff"} {
		if !id.MatchString(branch) {
			t.Fatalf("rehearsal branch %q is not a valid branch id", branch)
		}
		resource := "projects/p/branches/" + branch + "/endpoints/rw"
		if !Valid(resource) {
			t.Fatalf("endpoint %q of a rehearsal branch is not valid", resource)
		}
	}
	if strings.ToLower(RehearsalBranchPrefix) != RehearsalBranchPrefix {
		t.Fatalf("prefix %q has upper case; branch ids are lower case", RehearsalBranchPrefix)
	}
}
