package doctor

import (
	"regexp"
	"strings"
)

// maskedPassword replaces every password doctor finds in its text.
const maskedPassword = "****"

var (
	// urlPassword is the password part of a URL's user info.
	urlPassword = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^:/@\s]*:)[^@\s]*@`)
	// kvPassword is a key=value DSN's password, quoted or bare.
	kvPassword = regexp.MustCompile(`(?i)(password\s*=\s*)('[^']*'|\S+)`)
)

// Mask hides URL and key=value passwords and every given secret in s.
// Empty secrets are ignored.
func Mask(s string, secrets []string) string {
	s = urlPassword.ReplaceAllString(s, "${1}"+maskedPassword+"@")
	s = kvPassword.ReplaceAllString(s, "${1}"+maskedPassword)
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, maskedPassword)
		}
	}
	return s
}
