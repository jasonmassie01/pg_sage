package config

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// G5-B22: DSN() interpolated an unquoted password into key/value conninfo,
// so a space broke the connection and "x sslmode=disable" downgraded TLS.
func TestPostgresDSN_QuotesKeyValueFields(t *testing.T) {
	cases := []string{
		"p@ss word",
		"x sslmode=disable",
		`it's\complicated`,
		"",
	}
	for _, password := range cases {
		p := PostgresConfig{
			Host: "db.example", Port: 5433, User: "sage agent",
			Password: password, Database: "app", SSLMode: "verify-full",
		}
		parsed, err := pgconn.ParseConfig(p.DSN())
		if err != nil {
			t.Fatalf("password %q: DSN does not parse: %v", password, err)
		}
		if parsed.Password != password {
			t.Errorf("password = %q, want %q", parsed.Password, password)
		}
		if parsed.User != "sage agent" || parsed.Database != "app" ||
			parsed.Host != "db.example" || parsed.Port != 5433 {
			t.Errorf("identity = %s@%s:%d/%s", parsed.User, parsed.Host,
				parsed.Port, parsed.Database)
		}
		if parsed.TLSConfig == nil {
			t.Errorf("password %q downgraded sslmode=verify-full", password)
		}
	}
}

func TestPostgresDSN_DatabaseURLWins(t *testing.T) {
	p := PostgresConfig{DatabaseURL: "postgres://u@h/db", Host: "other"}
	if got := p.DSN(); got != "postgres://u@h/db" {
		t.Fatalf("DSN() = %q, want the configured URL", got)
	}
}
