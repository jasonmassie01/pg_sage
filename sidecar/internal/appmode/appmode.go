// Package appmode decides, from the process environment alone, whether
// pg_sage runs inside Databricks Apps, so the SDK pinning, the config policy
// and the authentication boundary all read one predicate.
package appmode

import (
	"errors"
)

const (
	// PortEnv is the variable the Databricks Apps platform injects.
	PortEnv = "DATABRICKS_APP_PORT"
	// AuthModeEnv selects pg_sage's dashboard authentication mode.
	AuthModeEnv = "SAGE_AUTH_MODE"
	// AuthMode is the AuthModeEnv value that trusts the Apps proxy identity.
	AuthMode = "databricks_app"
)

// ErrAppAuthRequired reports a binary started inside Databricks Apps without
// App auth mode, which would otherwise fall back to local password auth.
var ErrAppAuthRequired = errors.New("running inside Databricks Apps (" + PortEnv +
	" is set) requires " + AuthModeEnv + "=" + AuthMode)

// InApps reports whether either Apps signal is present: the runtime port the
// platform injects or the App auth mode the launcher selects. Either one pins
// the stricter App policy; Required makes a lone port fatal at startup.
func InApps(getenv func(string) string) bool {
	return getenv(PortEnv) != "" || getenv(AuthModeEnv) == AuthMode
}

// Required fails closed when the Apps runtime port is set without App auth
// mode. The port's value is validated by the App auth loader.
func Required(getenv func(string) string) error {
	if getenv(PortEnv) != "" && getenv(AuthModeEnv) != AuthMode {
		return ErrAppAuthRequired
	}
	return nil
}
