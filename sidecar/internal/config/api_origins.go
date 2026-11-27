package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// validateTrustedOrigins requires every api.trusted_origins entry to be an
// exact browser origin, scheme://host[:port] with an http or https scheme
// and nothing else, so the cross-origin check can compare it to the
// browser's Origin header byte for byte (apiauth-10).
func (a APIConfig) validateTrustedOrigins() error {
	for i, origin := range a.TrustedOrigins {
		if reason := originProblem(origin); reason != "" {
			return fmt.Errorf("api.trusted_origins[%d] %q: %s; want scheme://host[:port], "+
				"for example https://sage.example.com", i, origin, reason)
		}
	}
	return nil
}

// originProblem says what is wrong with one trusted-origin entry, or ""
// when it is a valid origin.
func originProblem(origin string) string {
	if strings.TrimSpace(origin) != origin || origin == "" {
		return "empty or padded with spaces"
	}
	u, err := url.Parse(origin)
	if err != nil {
		return "not a URL"
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "scheme must be http or https"
	case u.Host == "" || u.Hostname() == "":
		return "host is required"
	case strings.HasSuffix(u.Host, ":"):
		return "port is empty"
	case u.User != nil:
		return "user information is not allowed"
	case u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" ||
		strings.ContainsAny(origin, "?#"):
		return "path, query and fragment are not allowed"
	case strings.ToLower(origin) != origin:
		return "must be lowercase, as browsers send it"
	case (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80"):
		return "omit the default port, as browsers do"
	}
	return ""
}

// overlayAPIOriginsEnv reads SAGE_API_TRUSTED_ORIGINS, a comma-separated
// list that replaces api.trusted_origins when set and non-empty.
func overlayAPIOriginsEnv(cfg *Config) {
	raw := os.Getenv("SAGE_API_TRUSTED_ORIGINS")
	if strings.TrimSpace(raw) == "" {
		return
	}
	var origins []string
	for _, part := range strings.Split(raw, ",") {
		if origin := strings.TrimSpace(part); origin != "" {
			origins = append(origins, origin)
		}
	}
	cfg.API.TrustedOrigins = origins
}
