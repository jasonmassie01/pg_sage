package config

import (
	"fmt"
	"slices"
	"strings"
)

// Verification v2's switches (lab-01c). verify.method picks the verifier
// that decides retain or revert; verify.on_expired what happens to a
// monitor whose window ended while pg_sage was not watching.
const (
	DefaultVerifyMethod    = "auto"
	DefaultVerifyOnExpired = "ask"
)

// VerifyMethods and VerifyOnExpired are the accepted values.
var (
	VerifyMethods   = []string{"v1", "shadow", "v2", "auto"}
	VerifyOnExpired = []string{"ask", "keep", "revert"}
)

// validateMethod refuses a verifier or expiry policy outside the allowed
// values, naming them: an unknown value never falls back to a default.
func (v VerifyConfig) validateMethod() error {
	if !slices.Contains(VerifyMethods, v.Method) {
		return fmt.Errorf("verify.method must be one of %s, got %q",
			strings.Join(VerifyMethods, ", "), v.Method)
	}
	if !slices.Contains(VerifyOnExpired, v.OnExpired) {
		return fmt.Errorf("verify.on_expired must be one of %s, got %q",
			strings.Join(VerifyOnExpired, ", "), v.OnExpired)
	}
	return nil
}
