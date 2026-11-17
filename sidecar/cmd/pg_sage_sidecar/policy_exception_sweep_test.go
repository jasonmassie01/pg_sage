package main

import (
	"reflect"
	"testing"
)

// fleetpol-01a: fleet tags reach sage.databases as the map selectors match.
// No concurrent access: fleetTagMap is pure.
func TestFleetTagMap_ParsesKeyValueTags(t *testing.T) {
	cases := []struct {
		in   []string
		want map[string]string
	}{
		{[]string{"tier=prod", "team = pay"}, map[string]string{"tier": "prod", "team": "pay"}},
		{[]string{"canary"}, map[string]string{"canary": ""}},
		{[]string{"a=b=c"}, map[string]string{"a": "b=c"}},
		{[]string{"=orphan", "  "}, map[string]string{}},
		{nil, map[string]string{}},
		{[]string{"tier=dev", "tier=prod"}, map[string]string{"tier": "prod"}},
	}
	for _, tc := range cases {
		if got := fleetTagMap(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("fleetTagMap(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
