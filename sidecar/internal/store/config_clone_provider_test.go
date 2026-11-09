package store

import "testing"

// migration-07: the runtime config API refuses clone.provider snapshot,
// which has no adapter in this build.
func TestValidateConfigValueCloneProvider(t *testing.T) {
	for value, ok := range map[string]bool{"none": true, "dle": true, "snapshot": false,
		"": false, "magic": false} {
		err := validateConfigValue("clone.provider", value)
		if (err == nil) != ok {
			t.Errorf("clone.provider=%q: err %v, want accepted=%v", value, err, ok)
		}
	}
}
