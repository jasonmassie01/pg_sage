package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/notify"
)

// market-14: Teams, Opsgenie and incident.io are notification channel
// types. A channel that cannot deliver is refused at write time with a
// typed ErrValidation, not at 3 a.m. when the first alert fails.
// No concurrent access tests: pure validation functions.

func TestValidateChannelType_NewDestinations(t *testing.T) {
	for _, typ := range []string{"teams", "opsgenie", "incidentio"} {
		if err := validateChannelType(typ); err != nil {
			t.Errorf("validateChannelType(%q) = %v, want nil", typ, err)
		}
	}
	for _, typ := range []string{"Teams", "msteams", "incident.io", "OpsGenie", "servicenow"} {
		if err := validateChannelType(typ); !errors.Is(err, ErrValidation) {
			t.Errorf("validateChannelType(%q) = %v, want ErrValidation", typ, err)
		}
	}
	err := validateChannelType("sms")
	for _, name := range []string{"teams", "opsgenie", "incidentio"} {
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("error %v does not list %s", err, name)
		}
	}
}

func TestValidateChannelConfig_Teams(t *testing.T) {
	good := map[string]string{"webhook_url": "https://prod-1.westus.logic.azure.com/wf/1"}
	if err := validateChannelConfig("teams", good); err != nil {
		t.Fatalf("valid teams config: %v", err)
	}
	withLink := map[string]string{"webhook_url": good["webhook_url"],
		"dashboard_url": "https://sage.example.com"}
	if err := validateChannelConfig("teams", withLink); err != nil {
		t.Fatalf("teams config with dashboard_url: %v", err)
	}
	bad := map[string]map[string]string{
		"missing url":    {},
		"plain http":     {"webhook_url": "http://prod-1.westus.logic.azure.com/wf/1"},
		"no scheme":      {"webhook_url": "prod-1.westus.logic.azure.com/wf/1"},
		"bad link":       {"webhook_url": good["webhook_url"], "dashboard_url": "javascript:x"},
		"relative link":  {"webhook_url": good["webhook_url"], "dashboard_url": "/cases"},
		"whitespace url": {"webhook_url": "   "},
	}
	for name, cfg := range bad {
		if err := validateChannelConfig("teams", cfg); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", name, err)
		}
	}
}

func TestValidateChannelConfig_Opsgenie(t *testing.T) {
	for _, region := range []string{"", "us", "eu", "EU"} {
		cfg := map[string]string{"api_key": "k", "region": region}
		if err := validateChannelConfig("opsgenie", cfg); err != nil {
			t.Errorf("region %q: %v", region, err)
		}
	}
	bad := map[string]map[string]string{
		"missing api_key": {"region": "us"},
		"empty api_key":   {"api_key": ""},
		"bad region":      {"api_key": "k", "region": "apac"},
	}
	for name, cfg := range bad {
		err := validateChannelConfig("opsgenie", cfg)
		if !errors.Is(err, ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", name, err)
		}
	}
	err := validateChannelConfig("opsgenie", map[string]string{"api_key": "k", "region": "apac"})
	if err == nil || !strings.Contains(err.Error(), "us or eu") {
		t.Fatalf("bad region error = %v, want it to name us or eu", err)
	}
}

func TestValidateChannelConfig_IncidentIO(t *testing.T) {
	good := map[string]string{"token": "t", "alert_source_config_id": "01HXYZ"}
	if err := validateChannelConfig("incidentio", good); err != nil {
		t.Fatalf("valid incidentio config: %v", err)
	}
	bad := map[string]map[string]string{
		"missing token":     {"alert_source_config_id": "01HXYZ"},
		"missing source id": {"token": "t"},
		"path in source id": {"token": "t", "alert_source_config_id": "a/../b"},
	}
	for name, cfg := range bad {
		if err := validateChannelConfig("incidentio", cfg); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", name, err)
		}
	}
}

func TestValidateChannelConfig_NewDestinationsNilConfig(t *testing.T) {
	for _, typ := range []string{"teams", "opsgenie", "incidentio"} {
		if err := validateChannelConfig(typ, nil); !errors.Is(err, ErrValidation) {
			t.Errorf("%s with nil config: err = %v, want ErrValidation", typ, err)
		}
	}
}

// CHECK-02: a private-address Teams URL is refused unless
// allow_private_targets is set.
func TestValidateChannelTargets_TeamsPrivateURL(t *testing.T) {
	cfg := map[string]string{"webhook_url": "https://10.0.0.5/workflows/1"}
	if err := validateChannelTargets(notify.TargetPolicy{}, "teams", cfg); !errors.Is(err,
		ErrValidation) {
		t.Fatalf("strict policy accepted a private Teams URL: %v", err)
	}
	meta := map[string]string{"webhook_url": "https://169.254.169.254/latest"}
	if err := validateChannelTargets(notify.TargetPolicy{AllowPrivate: true}, "teams",
		meta); !errors.Is(err, ErrValidation) {
		t.Fatalf("allow_private_targets accepted the metadata address: %v", err)
	}
	if err := validateChannelTargets(notify.TargetPolicy{AllowPrivate: true}, "teams",
		cfg); err != nil {
		t.Fatalf("allow_private_targets refused a private Teams URL: %v", err)
	}
}

func TestNotificationSecretKeys_CoverNewDestinations(t *testing.T) {
	keys := map[string]bool{}
	for _, k := range notificationSecretKeys {
		keys[k] = true
	}
	for _, k := range []string{"webhook_url", "api_key", "token"} {
		if !keys[k] {
			t.Errorf("secret key %q is not sealed at rest", k)
		}
	}
	if keys["region"] || keys["alert_source_config_id"] || keys["dashboard_url"] {
		t.Error("a non-secret routing value is treated as a secret")
	}
}
