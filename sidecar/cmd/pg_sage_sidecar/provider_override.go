package main

import "fmt"

// resolveProviderOverride applies an operator's provider: override, which can
// only narrow: it is used while the probes say unknown, and it is a no-op
// when it agrees with them. A disagreement keeps the probed provider (the
// server's own answer) and returns a warning; so self-managed never
// replaces a detected managed provider.
func resolveProviderOverride(probed, override string) (string, string) {
	switch {
	case override == "" || override == probed:
		return probed, ""
	case probed == providerUnknown:
		return override, ""
	}
	return probed, fmt.Sprintf("configured provider %s disagrees with the detected "+
		"provider %s; pg_sage keeps %s (an override applies only while detection "+
		"cannot tell)", override, probed, probed)
}

// needsProviderReprobe reports whether the probes never got a definite
// answer, even when a configured override is in use.
func (rt *databaseRuntime) needsProviderReprobe() bool {
	return rt.probed == providerUnknown
}
