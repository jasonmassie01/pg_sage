package agentdb

import (
	"strings"
	"testing"
)

func TestProviderResourceName(t *testing.T) {
	cases := []struct {
		provider string
		id       string
		want     string
	}{
		{ProviderAWSRDS, "99999999-abcd", "pgsage-99999999-abcd"},
		{ProviderGCPCloudSQL, "Agent_DB_Test", "pgsage-agent-db-test"},
		{ProviderDatabricksLakebase, "a!b@c", "pgsage-a-b-c"},
	}
	for _, tc := range cases {
		got, err := ProviderResourceName(tc.provider, tc.id)
		if err != nil {
			t.Fatalf("ProviderResourceName(%s): %v", tc.provider, err)
		}
		if got != tc.want {
			t.Fatalf("name = %q, want %q", got, tc.want)
		}
		if len(got) > 63 || strings.Contains(got, "_") {
			t.Fatalf("invalid provider name = %q", got)
		}
	}
}

func TestProvisionStateTransitions(t *testing.T) {
	valid := [][2]string{
		{"planned", "preflight_passed"},
		{"planned", "preflight_failed"},
		{"preflight_passed", "provisioning"},
		{"provisioning", "available"},
		{"available", "destroy_pending"},
		{"destroy_pending", "destroying"},
		{"destroying", "destroyed"},
		{"queued", "cancel_requested"},
		{"cancel_requested", "cancelling"},
		{"provisioning", "status_unknown"},
		{"status_unknown", "provisioning"},
	}
	for _, item := range valid {
		if !validProvisionTransition(item[0], item[1]) {
			t.Fatalf("transition %s -> %s rejected", item[0], item[1])
		}
	}
	if validProvisionTransition("planned", "destroying") {
		t.Fatal("planned -> destroying should be rejected")
	}
}
