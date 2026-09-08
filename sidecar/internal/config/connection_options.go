package config

import "net/url"

// SetRuntimeConnectionOptions preserves transport/session options from an explicit secret DSN.
// The encoded copy is private, immutable and excluded from JSON/YAML persistence.
// Endpoint identity is always reconstructed from the resolved DatabaseConfig fields.
func (d *DatabaseConfig) SetRuntimeConnectionOptions(values url.Values) {
	copy := make(url.Values, len(values))
	for key, entries := range values {
		switch key {
		case "host", "hostaddr", "port", "user", "password", "dbname", "database", "sslmode":
			continue
		}
		copy[key] = append([]string(nil), entries...)
	}
	d.connectionOptions = copy.Encode()
}
