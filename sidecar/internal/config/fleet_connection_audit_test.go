package config

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestAuditFleetConnectionPreservesFields(t *testing.T) {
	// No concurrent access tests: ConnString is stateless and takes a value receiver.
	cases := []struct {
		name string
		db   DatabaseConfig
	}{
		{"reserved characters", DatabaseConfig{
			Host: "localhost", Port: 5455, User: "audit@user", Password: "a@b?#%/:",
			Database: "audit?sslmode=require#name", SSLMode: "disable"}},
		{"IPv6", DatabaseConfig{
			Host: "::1", Port: 5455, User: "audit", Password: "synthetic",
			Database: "audit", SSLMode: "disable"}},
		{"leading slash database", DatabaseConfig{
			Host: "localhost", Port: 5455, User: "audit", Password: "synthetic",
			Database: "/audit", SSLMode: "disable"}},
		{"empty password and default SSL", DatabaseConfig{
			Host: "localhost", Port: 5455, User: "audit", Database: "audit"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := pgx.ParseConfig(tc.db.ConnString())
			if err != nil {
				t.Fatal("generated connection string is not parseable")
			}
			if parsed.User != tc.db.User || parsed.Password != tc.db.Password {
				t.Fatal("connection string changed its credentials")
			}
			if parsed.Database != tc.db.Database || parsed.Host != tc.db.Host ||
				int(parsed.Port) != tc.db.Port {
				t.Fatal("connection string changed its target")
			}
			if tc.db.SSLMode == "disable" && parsed.TLSConfig != nil {
				t.Fatal("database text overrode the selected SSL mode")
			}
		})
	}
}
