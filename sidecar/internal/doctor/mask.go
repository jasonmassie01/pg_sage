package doctor

import (
	"regexp"
	"strings"
)

// maskedPassword replaces every password doctor finds in its text.
const maskedPassword = "****"

var (
	// urlPassword is the password part of a URL's user info, up to the
	// last '@' of the token, as URL parsers split it.
	urlPassword = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^:/@\s]*:)\S*@`)
	// kvPassword is a key=value DSN's password, quoted (with backslash
	// escapes) or bare.
	kvPassword = regexp.MustCompile(`(?i)(password\s*=\s*)('(?:[^'\\]|\\.)*'|\S+)`)
)

// Mask hides every given secret, then URL and key=value passwords, in s.
// The secrets go first so a pattern never cuts one in half. Empty secrets
// are ignored.
func Mask(s string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, maskedPassword)
		}
	}
	s = urlPassword.ReplaceAllString(s, "${1}"+maskedPassword+"@")
	return kvPassword.ReplaceAllString(s, "${1}"+maskedPassword)
}
