package store

import (
	"fmt"
	"strings"
)

// smtpPasswordKeys are the channel keys that hold the SMTP relay password.
var smtpPasswordKeys = []string{"smtp_pass", "smtp_password"}

// checkSMTPDestinationSecrets refuses an update that moves an email channel
// to another relay (smtp_host or smtp_port) while sending the masked stored
// password: keeping it would hand the password to the new relay.
func checkSMTPDestinationSecrets(existing, incoming map[string]string) error {
	if incoming == nil || smtpRelay(existing) == smtpRelay(incoming) {
		return nil
	}
	for _, key := range smtpPasswordKeys {
		if existing[key] != "" && isMaskedNotificationSecret(incoming[key]) {
			return fmt.Errorf("%w: smtp_host or smtp_port changed: re-enter %s "+
				"for the new relay, or send it empty to clear it", ErrValidation, key)
		}
	}
	return nil
}

// smtpRelay is the relay a config dials; an empty port means 587, as in
// notify's email sender.
func smtpRelay(cfg map[string]string) string {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(cfg["smtp_host"])), ".")
	port := strings.TrimSpace(cfg["smtp_port"])
	if port == "" {
		port = "587"
	}
	return host + ":" + port
}
