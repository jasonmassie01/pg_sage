package config

import (
	"strings"
	"testing"
)

// alerting-07: alerting.repeat_minutes sets how often a still-open
// finding is re-sent: critical 240, warning 1440, info 0 (0 = never).
// A missing key takes its default; a negative value, an unknown severity
// or a value below the severity's cooldown floor fails load naming the key.
// No concurrency test: the config is a value loaded once per process.

const repeatRoute = "  routes:\n    - severity: critical\n      channels: [slack]\n"

func loadRepeatYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	return loadAlertingYAML(t, "  enabled: true\n"+alertSlackURL+body+repeatRoute)
}

func wantRepeat(t *testing.T, label string, got map[string]int, crit, warn, info int) {
	t.Helper()
	if len(got) != 3 || got["critical"] != crit || got["warning"] != warn ||
		got["info"] != info {
		t.Errorf("%s repeat = %v, want critical %d warning %d info %d",
			label, got, crit, warn, info)
	}
}

// Default-value masking guard: no repeat_minutes gives 240/1440/0.
func TestAlertingRepeat_DefaultsWithoutKey(t *testing.T) {
	cfg, err := loadRepeatYAML(t, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	wantRepeat(t, "loaded", cfg.Alerting.EffectiveRepeatMinutes(), 240, 1440, 0)
	wantRepeat(t, "Defaults()", defaultConfigRepeat(), 240, 1440, 0)
	wantRepeat(t, "nil map", ResolveAlertRepeatMinutes(nil), 240, 1440, 0)
}

func defaultConfigRepeat() map[string]int {
	cfg := DefaultConfig()
	return cfg.Alerting.EffectiveRepeatMinutes()
}

func TestAlertingRepeat_MissingKeysKeepDefaults(t *testing.T) {
	cfg, err := loadRepeatYAML(t, "  repeat_minutes:\n    critical: 60\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	wantRepeat(t, "critical only", cfg.Alerting.EffectiveRepeatMinutes(), 60, 1440, 0)
}

func TestAlertingRepeat_ZeroAndFloorsAccepted(t *testing.T) {
	cfg, err := loadRepeatYAML(t, "  cooldown_minutes: 0\n  repeat_minutes:\n"+
		"    critical: 5\n    warning: 30\n    info: 360\n")
	if err != nil {
		t.Fatalf("values at the floors refused: %v", err)
	}
	wantRepeat(t, "floors", cfg.Alerting.EffectiveRepeatMinutes(), 5, 30, 360)
	cfg, err = loadRepeatYAML(t, "  repeat_minutes:\n    critical: 0\n    warning: 0\n")
	if err != nil {
		t.Fatalf("zero refused: %v", err)
	}
	wantRepeat(t, "zeros", cfg.Alerting.EffectiveRepeatMinutes(), 0, 0, 0)
}

func requireRepeatErr(t *testing.T, body string, wants ...string) {
	t.Helper()
	requireAlertingErr(t, "  enabled: true\n"+alertSlackURL+body+repeatRoute, wants...)
}

func TestAlertingRepeat_NegativeRefusedNamingKey(t *testing.T) {
	requireRepeatErr(t, "  repeat_minutes:\n    critical: -5\n",
		"alerting.repeat_minutes.critical", "-5")
}

func TestAlertingRepeat_BelowFiveMinuteFloorRefused(t *testing.T) {
	requireRepeatErr(t, "  cooldown_minutes: 0\n  repeat_minutes:\n    critical: 2\n",
		"alerting.repeat_minutes.critical", "2", "5 minutes")
}

// cooldown_minutes raises every floor, so 10 is below the default 15.
func TestAlertingRepeat_CooldownMinutesRaisesFloor(t *testing.T) {
	requireRepeatErr(t, "  repeat_minutes:\n    critical: 10\n",
		"alerting.repeat_minutes.critical", "15 minutes", "alerting.cooldown_minutes")
	if _, err := loadRepeatYAML(t, "  repeat_minutes:\n    critical: 15\n"); err != nil {
		t.Fatalf("critical 15 at the 15-minute floor refused: %v", err)
	}
}

func TestAlertingRepeat_WarningAndInfoFloors(t *testing.T) {
	requireRepeatErr(t, "  repeat_minutes:\n    warning: 29\n",
		"alerting.repeat_minutes.warning", "30 minutes")
	requireRepeatErr(t, "  repeat_minutes:\n    info: 359\n",
		"alerting.repeat_minutes.info", "360 minutes")
}

func TestAlertingRepeat_UnknownSeverityRefused(t *testing.T) {
	requireRepeatErr(t, "  repeat_minutes:\n    high: 60\n",
		"alerting.repeat_minutes.high", "critical, warning, info")
}

func TestAlertingRepeat_UpperBound(t *testing.T) {
	if _, err := loadRepeatYAML(t, "  repeat_minutes:\n    warning: 525600\n"); err != nil {
		t.Fatalf("one year refused: %v", err)
	}
	requireRepeatErr(t, "  repeat_minutes:\n    warning: 525601\n",
		"alerting.repeat_minutes.warning", "525600")
}

func TestAlertingRepeat_NotNumberRefused(t *testing.T) {
	_, err := loadRepeatYAML(t, "  repeat_minutes:\n    critical: often\n")
	if err == nil || !strings.Contains(err.Error(), "often") {
		t.Fatalf("err = %v, want a parse error naming the value", err)
	}
}

// A disabled block is not validated, like the rest of alerting.
func TestAlertingRepeat_DisabledBlockNotValidated(t *testing.T) {
	cfg, err := loadAlertingYAML(t, "  enabled: false\n  repeat_minutes:\n    critical: -5\n")
	if err != nil {
		t.Fatalf("disabled block validated: %v", err)
	}
	if cfg.Alerting.RepeatMinutes["critical"] != -5 {
		t.Errorf("repeat = %v, want the value kept", cfg.Alerting.RepeatMinutes)
	}
}

func TestAlertingRepeat_CloneAndResolveDoNotAlias(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Alerting.RepeatMinutes = map[string]int{"critical": 60}
	cp := Clone(cfg)
	cp.Alerting.RepeatMinutes["critical"] = 120
	if cfg.Alerting.RepeatMinutes["critical"] != 60 {
		t.Errorf("Clone shares RepeatMinutes: original = %v", cfg.Alerting.RepeatMinutes)
	}
	resolved := ResolveAlertRepeatMinutes(cfg.Alerting.RepeatMinutes)
	resolved["critical"] = 1
	if cfg.Alerting.RepeatMinutes["critical"] != 60 {
		t.Errorf("Resolve returned the input map: %v", cfg.Alerting.RepeatMinutes)
	}
	again := ResolveAlertRepeatMinutes(nil)
	again["warning"] = 1
	wantRepeat(t, "fresh defaults", ResolveAlertRepeatMinutes(nil), 240, 1440, 0)
}

func TestAlertCooldownFloorMinutes(t *testing.T) {
	cases := []struct {
		sev      string
		cooldown int
		want     int
	}{
		{"critical", 0, 5}, {"warning", 0, 30}, {"info", 0, 360},
		{"critical", 15, 15}, {"warning", 15, 30}, {"info", 600, 600},
		{"other", 0, 360}, {"critical", -3, 5},
	}
	for _, tc := range cases {
		if got := AlertCooldownFloorMinutes(tc.sev, tc.cooldown); got != tc.want {
			t.Errorf("AlertCooldownFloorMinutes(%s, %d) = %d, want %d",
				tc.sev, tc.cooldown, got, tc.want)
		}
	}
}

// The repeat interval is the operator's notification preference: never
// derived, restart-bound like alerting.cooldown_minutes.
func TestAlertingRepeat_IsOperatorPreference(t *testing.T) {
	class, ok := KeyClassOf("alerting.repeat_minutes")
	if !ok || class != KeyOperatorPreference {
		t.Errorf("alerting.repeat_minutes = %q (%v), want operator_preference", class, ok)
	}
}
