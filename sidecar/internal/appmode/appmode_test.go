package appmode

import (
	"errors"
	"strings"
	"testing"
)

// No concurrent access tests: both functions are pure reads of the getenv
// callback, which startup consults once.

func envOf(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestInAppsSignals(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"neither variable", nil, false},
		{"session outside Apps", map[string]string{AuthModeEnv: "session"}, false},
		{"unknown mode outside Apps", map[string]string{AuthModeEnv: "proxy"}, false},
		{"runtime port", map[string]string{PortEnv: "8000"}, true},
		{"invalid runtime port is still a port", map[string]string{PortEnv: "abc"}, true},
		{"zero runtime port is still a port", map[string]string{PortEnv: "0"}, true},
		{"App auth mode", map[string]string{AuthModeEnv: AuthMode}, true},
		{"both signals", map[string]string{PortEnv: "8000", AuthModeEnv: AuthMode}, true},
		{"mode is case sensitive", map[string]string{AuthModeEnv: "DATABRICKS_APP"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := InApps(envOf(tc.env)); got != tc.want {
				t.Fatalf("InApps(%v) = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

func TestRequiredFailsClosedWithoutAppAuthMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"port without mode", map[string]string{PortEnv: "8000"}},
		{"port with session mode", map[string]string{PortEnv: "8000", AuthModeEnv: "session"}},
		{"port with unknown mode", map[string]string{PortEnv: "8000", AuthModeEnv: "proxy"}},
		{"port with differently cased mode", map[string]string{
			PortEnv: "8000", AuthModeEnv: "Databricks_App",
		}},
		{"unparsable port without mode", map[string]string{PortEnv: "abc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Required(envOf(tc.env))
			if !errors.Is(err, ErrAppAuthRequired) {
				t.Fatalf("Required(%v) = %v, want ErrAppAuthRequired", tc.env, err)
			}
			for _, want := range []string{PortEnv, AuthModeEnv + "=" + AuthMode} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %s", err, want)
				}
			}
		})
	}
}

func TestRequiredAllowsConsistentEnvironments(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"empty environment", nil},
		{"session outside Apps", map[string]string{AuthModeEnv: "session"}},
		{"App mode inside Apps", map[string]string{PortEnv: "8000", AuthModeEnv: AuthMode}},
		// The port's value is the auth loader's concern; Required only
		// pairs the runtime signal with the mode.
		{"App mode with invalid port", map[string]string{PortEnv: "0", AuthModeEnv: AuthMode}},
		{"App mode without port", map[string]string{AuthModeEnv: AuthMode}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Required(envOf(tc.env)); err != nil {
				t.Fatalf("Required(%v) = %v, want nil", tc.env, err)
			}
		})
	}
}
