package startup

import (
	"errors"
	"strings"
)

// ValidateSessionEndpoint rejects known transaction poolers. Bootstrap advisory
// locks and session-local optimizer state require a stable backend connection.
func ValidateSessionEndpoint(host string, port uint16) error {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	firstLabel, _, _ := strings.Cut(host, ".")
	if strings.HasSuffix(host, ".neon.tech") && strings.HasSuffix(firstLabel, "-pooler") {
		return errors.New("pg_sage requires a Neon direct endpoint; " +
			"disable connection pooling in the Neon connection dialog")
	}
	shared := strings.HasSuffix(host, ".pooler.supabase.com")
	dedicated := strings.HasPrefix(host, "db.") && strings.HasSuffix(host, ".supabase.co")
	if (shared || dedicated) && port == 6543 {
		return errors.New("pg_sage requires a Supabase direct connection or " +
			"session pooler on port 5432; transaction pooling is not session-safe")
	}
	return nil
}
