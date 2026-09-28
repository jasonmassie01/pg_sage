package main

import "strings"

// hostedProviderFromHost recognizes official hosted endpoints. Custom domains
// still fall through to database probes; a hostname never grants permissions.
func hostedProviderFromHost(host string) string {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if strings.ContainsAny(host, "/:@\\ \t\r\n") {
		return ""
	}
	switch {
	case strings.HasSuffix(host, ".neon.tech"):
		return "neon"
	case strings.HasSuffix(host, ".supabase.co"),
		strings.HasSuffix(host, ".pooler.supabase.com"):
		return "supabase"
	case strings.HasSuffix(host, ".postgres.database.azure.com"):
		return "azure"
	case strings.HasSuffix(host, ".postgres.cosmos.azure.com"):
		return "azure-cosmos" // Cosmos DB for PostgreSQL (Citus) cluster
	default:
		return ""
	}
}
