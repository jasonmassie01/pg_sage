package appmode_test

import (
	"os"
	"testing"

	"github.com/pg-sage/sidecar/internal/appmode"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/databricksauth"
)

// TestAppModePredicatesAgree drives every "am I inside Databricks Apps"
// predicate from one table so the SDK pinning, config policy and auth
// boundary can never disagree again.
func TestAppModePredicatesAgree(t *testing.T) {
	for _, tc := range []struct {
		name, port, mode string
		want             bool
	}{
		{"neither", "", "", false},
		{"session outside Apps", "", "session", false},
		{"port only", "8000", "", true},
		{"port with session", "8000", "session", true},
		{"invalid port", "abc", "", true},
		{"mode only", "", appmode.AuthMode, true},
		{"both", "8000", appmode.AuthMode, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(appmode.PortEnv, tc.port)
			t.Setenv(appmode.AuthModeEnv, tc.mode)
			got := map[string]bool{
				"appmode.InApps":           appmode.InApps(os.Getenv),
				"databricksauth.AppMode":   databricksauth.AppMode(),
				"config.DatabricksAppMode": config.DatabricksAppMode(),
			}
			for name, value := range got {
				if value != tc.want {
					t.Errorf("%s = %v, want %v (port=%q mode=%q)",
						name, value, tc.want, tc.port, tc.mode)
				}
			}
		})
	}
}
