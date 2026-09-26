package store

import "testing"

// G7-B21: the notification target policy must not be an API override;
// otherwise any admin session could re-open SSRF to private networks.
func TestNotificationPolicyIsNotAnAPIOverride(t *testing.T) {
	const key = "notification_policy.allow_private_targets"
	if err := ValidateConfigOverride(key, "true"); err == nil {
		t.Fatalf("%s accepted as an API config override", key)
	}
	if _, ok := AllowedConfigKeysSnapshot()[key]; ok {
		t.Fatalf("%s is listed in allowedConfigKeys", key)
	}
}
