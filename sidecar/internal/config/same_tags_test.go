package config

import "testing"

// fleet-01: SameTags is the exported tag-set comparison meta-DB reconcile
// shares with the YAML lifecycle; nil and empty are the same set.
func TestSameTags(t *testing.T) {
	cases := []struct {
		name        string
		left, right []string
		want        bool
	}{
		{"both nil", nil, nil, true},
		{"nil and empty", nil, []string{}, true},
		{"equal", []string{"tenant=acme"}, []string{"tenant=acme"}, true},
		{"different value", []string{"tenant=acme"}, []string{"tenant=globex"}, false},
		{"nil and one tag", nil, []string{"tenant=acme"}, false},
		{"extra tag", []string{"a=1"}, []string{"a=1", "b=2"}, false},
		{"order matters", []string{"a=1", "b=2"}, []string{"b=2", "a=1"}, false},
	}
	for _, tc := range cases {
		if got := SameTags(tc.left, tc.right); got != tc.want {
			t.Errorf("%s: SameTags(%v, %v) = %v, want %v",
				tc.name, tc.left, tc.right, got, tc.want)
		}
	}
}
