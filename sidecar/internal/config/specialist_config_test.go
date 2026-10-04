package config

import (
	"strings"
	"testing"
	"time"
)

// specialist.* (roadmap phase 3, the Postgres specialist other agents
// call): on by default (it is token-only, read and propose), bounded per
// identity, and adapters only call operator-configured endpoints.

func TestSpecialistDefaults_NoConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := cfg.Specialist
	if !s.Enabled || s.WritesPerMinute != 30 || s.ReadsPerMinute != 240 ||
		s.MaxOpenPerIdentity != 3 || s.MaxOpenTotal != 10 || !s.KeepIdentifiers ||
		s.Webhook.TimestampToleranceSeconds != 300 || s.PagerDuty.APIURL != "" ||
		len(s.PagerDuty.Services) != 0 || s.Webhook.ResultURL != "" {
		t.Fatalf("specialist defaults = %+v", s)
	}
	if s.Webhook.Tolerance() != 5*time.Minute {
		t.Fatalf("tolerance %s", s.Webhook.Tolerance())
	}
}

func TestSpecialistPartialSectionKeepsDefaults(t *testing.T) {
	cfg, err := loadRCAYAML(t, "specialist:\n  writes_per_minute: 5\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Specialist.WritesPerMinute != 5 || cfg.Specialist.ReadsPerMinute != 240 ||
		!cfg.Specialist.Enabled || !cfg.Specialist.KeepIdentifiers {
		t.Fatalf("partial = %+v", cfg.Specialist)
	}
}

func TestSpecialistAdaptersFromYAML(t *testing.T) {
	cfg, err := loadRCAYAML(t, "specialist:\n  keep_identifiers: false\n"+
		"  pagerduty:\n    signing_secret: s3cret\n    services: [\"PSVC1=orders:lock_blocking\"]\n"+
		"    api_url: https://api.pagerduty.com\n    api_token: tok\n"+
		"    from_email: sre@example.com\n"+
		"  webhook:\n    signing_secret: wh\n    result_url: https://hooks.example.com/sage\n"+
		"    timestamp_tolerance_seconds: 60\n")
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Specialist
	if s.KeepIdentifiers || s.PagerDuty.SigningSecret != "s3cret" ||
		s.PagerDuty.Services[0] != "PSVC1=orders:lock_blocking" ||
		s.PagerDuty.APIURL != "https://api.pagerduty.com" || s.Webhook.ResultURL !=
		"https://hooks.example.com/sage" || s.Webhook.Tolerance() != time.Minute {
		t.Fatalf("adapters = %+v", s)
	}
}

func TestSpecialistValidation(t *testing.T) {
	cases := map[string]string{
		"zero writes":       "specialist:\n  writes_per_minute: 0\n",
		"huge reads":        "specialist:\n  reads_per_minute: 100001\n",
		"zero per identity": "specialist:\n  max_open_per_identity: 0\n",
		"per over total":    "specialist:\n  max_open_per_identity: 11\n  max_open_total: 10\n",
		"total over 100":    "specialist:\n  max_open_total: 101\n",
		"services without secret": "specialist:\n  pagerduty:\n" +
			"    services: [\"PSVC1=orders\"]\n",
		"malformed service": "specialist:\n  pagerduty:\n    signing_secret: s\n" +
			"    services: [\"orders\"]\n",
		"plain http api": "specialist:\n  pagerduty:\n    api_url: http://api.pagerduty.com\n" +
			"    api_token: t\n    from_email: a@b.c\n",
		"api without token": "specialist:\n  pagerduty:\n    api_url: https://api.pagerduty.com\n" +
			"    from_email: a@b.c\n",
		"api without from": "specialist:\n  pagerduty:\n    api_url: https://api.pagerduty.com\n" +
			"    api_token: t\n",
		"result url scheme": "specialist:\n  webhook:\n    signing_secret: s\n" +
			"    result_url: ftp://x\n",
		"result url without secret": "specialist:\n  webhook:\n" +
			"    result_url: https://hooks.example.com\n",
		"tolerance too small": "specialist:\n  webhook:\n    timestamp_tolerance_seconds: 5\n",
		"tolerance too large": "specialist:\n  webhook:\n    timestamp_tolerance_seconds: 901\n",
		"unknown key":         "specialist:\n  force_remediations: true\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadRCAYAML(t, body)
			if err == nil {
				t.Fatalf("%s accepted", name)
			}
			if name != "unknown key" && !strings.Contains(err.Error(), "specialist.") {
				t.Fatalf("error does not name the key: %v", err)
			}
		})
	}
}

func TestSpecialistLoopbackHTTPIsAllowedForLocalEndpoints(t *testing.T) {
	cfg, err := loadRCAYAML(t, "specialist:\n  webhook:\n    signing_secret: s\n"+
		"    result_url: http://127.0.0.1:9000/hook\n")
	if err != nil || cfg.Specialist.Webhook.ResultURL != "http://127.0.0.1:9000/hook" {
		t.Fatalf("loopback http: %v", err)
	}
}

func TestSpecialistDisabled(t *testing.T) {
	cfg, err := loadRCAYAML(t, "specialist:\n  enabled: false\n")
	if err != nil || cfg.Specialist.Enabled {
		t.Fatalf("disabled: %+v %v", cfg.Specialist, err)
	}
}
