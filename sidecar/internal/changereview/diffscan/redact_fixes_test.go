package diffscan

import (
	"strings"
	"testing"
)

// The secrets migration SQL and config files carry most often: SQL's
// PASSWORD 'x' and OPTIONS (password 'x') forms, URL passwords holding '/'
// or '@', and unquoted values with punctuation. Code that only names a
// secret (a member, a variable, a template) is still kept.

func TestRedact_SQLAndPunctuatedSecrets(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"create role", "+CREATE ROLE reporting LOGIN PASSWORD 'Spr1ngT1me!';",
			"+CREATE ROLE reporting LOGIN PASSWORD '[redacted]';"},
		{"alter user", "+ALTER USER app WITH ENCRYPTED PASSWORD 'hunter2';",
			"+ALTER USER app WITH ENCRYPTED PASSWORD '[redacted]';"},
		{"user mapping", "+CREATE USER MAPPING FOR app SERVER s OPTIONS (user 'u', " +
			"password 'p4ss');", "+CREATE USER MAPPING FOR app SERVER s OPTIONS (user 'u', " +
			"password '[redacted]');"},
		{"quoted with a doubled quote", "PASSWORD 'it''s secret'", "PASSWORD '[redacted]'"},
		{"url password with a slash", "+DATABASE_URL=postgres://app:pa/ss@db:5432/x",
			"+DATABASE_URL=postgres://[redacted]@db:5432/x"},
		{"url password with an at sign", "postgres://app:p@ss@db/x",
			"postgres://[redacted]@db/x"},
		{"env value with a dot", "+PGPASSWORD=s3cr3t.2024", "+PGPASSWORD=[redacted]"},
		{"env value with a dollar", "+DB_PASSWORD=Hunter2$", "+DB_PASSWORD=[redacted]"},
		{"yaml value with an angle bracket", "password: hunt<er2", "password: [redacted]"},
		{"env value with punctuation", "+DATABASE_PASSWORD=p@ss.word!",
			"+DATABASE_PASSWORD=[redacted]"},
	}
	for _, c := range cases {
		if got := Redact(c.in); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestRedact_KeepsCodeThatNamesASecret(t *testing.T) {
	for _, in := range []string{
		"password = settings.DB_PASSWORD",
		"secret: process.env.SECRET",
		"password = $DB_PASSWORD",
		"password: ${DB_PASSWORD}",
		"password: <%= ENV['DB_PASSWORD'] %>",
		"password: {{ .Values.dbPassword }}",
		"Password: cfg.Password,",
		"password => params[:password]",
		"token := os.Getenv(\"TOKEN\")",
		"COMMENT ON COLUMN users.password IS 'bcrypt hash'",
		"t.string :password",
		"https://github.com/acme/shop/pull/42",
	} {
		if got := Redact(in); got != in {
			t.Errorf("Redact(%q) = %q, want it unchanged", in, got)
		}
	}
}

// Every redacted form stays redacted under a second pass, and no secret
// of the fixtures survives either pass.
func TestRedact_FixesAreIdempotent(t *testing.T) {
	in := "PASSWORD 'Spr1ngT1me!' postgres://app:p@ss@db/x PGPASSWORD=s3cr3t.2024 " +
		"OPTIONS (password 'p4ss')"
	once := Redact(in)
	if twice := Redact(once); twice != once {
		t.Fatalf("not idempotent:\n once %q\ntwice %q", once, twice)
	}
	for _, leak := range []string{"Spr1ngT1me", "p@ss@", "s3cr3t.2024", "p4ss"} {
		if strings.Contains(once, leak) {
			t.Errorf("%q leaked: %q", leak, once)
		}
	}
}
