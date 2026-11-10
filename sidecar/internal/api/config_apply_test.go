package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
)

// alerting-11: the config API refuses an alerting Slack URL that the
// notification target policy blocks (G7-B21), before persisting or
// hot-reloading it. No concurrency test: validation is a pure function of
// the request body and the YAML-only policy.

const oldSlackURL = "https://hooks.slack.com/services/T/B/old"

func slackConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Alerting.SlackWebhookURL = oldSlackURL
	return cfg
}

// The legacy apply path is gone (config-10); every global write is checked
// by boundConfigWrites before the controller persists or applies it.
func TestConfigWrites_LinkLocalSlackURLRefused(t *testing.T) {
	cfg := slackConfig()
	writes, errs := boundConfigWrites(map[string]any{
		"alerting.slack_webhook_url": "https://169.254.169.254/latest/meta-data",
	}, cfg)
	if len(writes) != 0 {
		t.Fatalf("writes = %+v, want none", writes)
	}
	if len(errs) != 1 || !strings.Contains(errs[0], "alerting.slack_webhook_url") ||
		!strings.Contains(errs[0], notify.ErrTargetBlocked.Error()) {
		t.Fatalf("errs = %q, want one policy refusal naming the key", errs)
	}
	if strings.Contains(errs[0], "meta-data") {
		t.Fatalf("error %q echoes the submitted URL", errs[0])
	}
	if cfg.Alerting.SlackWebhookURL != oldSlackURL {
		t.Fatalf("slack URL changed to %q", cfg.Alerting.SlackWebhookURL)
	}
}

func TestConfigPut_LinkLocalSlackURLReturns400(t *testing.T) {
	cfg := slackConfig()
	controller := config.NewConfigController(cfg, nil, apiTrustOwner{})
	handler := configGlobalPutHandler(nil, controller)
	w := doRequest(handler, "PUT", "/api/v1/config/global",
		`{"alerting.slack_webhook_url":"http://169.254.169.254/latest","expected_generation":1}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "blocked by policy") {
		t.Fatalf("body = %s, want the policy reason", w.Body.String())
	}
	if got := controller.Desired().Config.Alerting.SlackWebhookURL; got != oldSlackURL {
		t.Fatalf("slack URL changed to %q", got)
	}
}

func TestConfigPut_ControlledLinkLocalSlackURLReturns400(t *testing.T) {
	cfg := slackConfig()
	controller := config.NewConfigController(cfg, nil, apiTrustOwner{})
	before := controller.Desired().Generation
	handler := configGlobalPutHandler(nil, controller)
	w := doRequest(handler, "PUT", "/api/v1/config/global",
		`{"alerting.slack_webhook_url":"https://[fe80::1]/hook","expected_generation":1}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", w.Code, w.Body.String())
	}
	desired := controller.Desired()
	if desired.Generation != before ||
		desired.Config.Alerting.SlackWebhookURL != oldSlackURL {
		t.Fatalf("desired generation %d url %q, want unchanged %d %q",
			desired.Generation, desired.Config.Alerting.SlackWebhookURL, before, oldSlackURL)
	}
}

func TestConfigWrites_PrivateSlackURLNeedsAllowPrivate(t *testing.T) {
	body := map[string]any{"alerting.slack_webhook_url": "https://10.0.0.5/hook"}
	writes, errs := validatedConfigWrites(body, notify.TargetPolicy{})
	if len(writes) != 0 || len(errs) != 1 || !strings.Contains(errs[0], "private address") {
		t.Fatalf("strict policy: writes=%+v errs=%q, want one private-address refusal",
			writes, errs)
	}
	writes, errs = validatedConfigWrites(body, notify.TargetPolicy{AllowPrivate: true})
	if len(errs) != 0 || len(writes) != 1 || writes[0].Value != "https://10.0.0.5/hook" {
		t.Fatalf("allow private: writes=%+v errs=%q, want the write accepted", writes, errs)
	}
}

func TestConfigWrites_SlackURLBoundaries(t *testing.T) {
	cases := []struct {
		value string
		ok    bool
	}{
		{"", true}, // clearing the URL disables the channel
		{"https://hooks.slack.com/services/T/B/x", true},
		{"ftp://hooks.slack.com/x", false},
		{"https://metadata.google.internal/x", false},
		{"https://", false},
		{"http://hooks.slack.com/x", false},
	}
	for _, tc := range cases {
		writes, errs := validatedConfigWrites(
			map[string]any{"alerting.slack_webhook_url": tc.value}, notify.TargetPolicy{})
		if tc.ok && (len(errs) != 0 || len(writes) != 1 || writes[0].Value != tc.value) {
			t.Errorf("%q: writes=%+v errs=%q, want accepted", tc.value, writes, errs)
		}
		if !tc.ok && (len(writes) != 0 || len(errs) != 1) {
			t.Errorf("%q: writes=%+v errs=%q, want refused", tc.value, writes, errs)
		}
	}
}

func TestConfigWrites_OtherKeysIgnorePolicy(t *testing.T) {
	writes, errs := validatedConfigWrites(
		map[string]any{"alerting.cooldown_minutes": "30"}, notify.TargetPolicy{})
	if len(errs) != 0 || len(writes) != 1 || writes[0].Key != "alerting.cooldown_minutes" {
		t.Fatalf("writes=%+v errs=%q, want the cooldown write untouched", writes, errs)
	}
}
