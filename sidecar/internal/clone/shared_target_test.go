package clone

import "testing"

// benches-01: a clone provider gives each run its own target, so it is
// not a SharedTarget and runs on it are never serialized by a lease.
func TestCloneProvidersAreNotSharedTargets(t *testing.T) {
	for name, p := range map[string]Provider{
		"dle": &DLEProvider{}, "snapshot": &SnapshotProvider{},
	} {
		if _, ok := p.(SharedTarget); ok {
			t.Errorf("the %s provider is a shared target", name)
		}
	}
}
