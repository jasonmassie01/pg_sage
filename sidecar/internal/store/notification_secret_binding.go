package store

import (
	"fmt"
	"strings"
)

// secretTarget names the setting that says where a channel type sends
// its secrets, and the secrets sent there verbatim (market-14).
type secretTarget struct {
	target  string
	secrets []string
}

// secretTargets lists the types whose masked secrets would follow a moved
// target: a Jira integration's API token goes to its site, and webhook
// headers (bearer tokens) go to the webhook URL. Opsgenie and incident.io
// post only to their fixed APIs, and a Teams URL is its own secret.
var secretTargets = map[string]secretTarget{
	"jira":    {target: "base_url", secrets: []string{"api_token"}},
	"webhook": {target: "webhook_url", secrets: []string{"headers"}},
}

// checkDestinationSecrets refuses an update that moves a channel to
// another target while sending a masked stored secret back: keeping it
// would hand the secret to the new target, as checkSMTPDestinationSecrets
// does for email.
func checkDestinationSecrets(typ string, existing, incoming map[string]string) error {
	st, ok := secretTargets[typ]
	if !ok || incoming == nil {
		return nil
	}
	moved := incoming[st.target]
	if isMaskedNotificationSecret(moved) ||
		normalizedTarget(moved) == normalizedTarget(existing[st.target]) {
		return nil
	}
	for _, key := range st.secrets {
		if existing[key] != "" && isMaskedNotificationSecret(incoming[key]) {
			return fmt.Errorf("%w: %s changed: re-enter %s for the new target, or "+
				"send it empty to clear it", ErrValidation, st.target, key)
		}
	}
	return nil
}

// normalizedTarget compares targets without case, spaces or a trailing
// slash, so re-saving the same site is not a move.
func normalizedTarget(v string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(v)), "/")
}
