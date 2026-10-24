package diffscan

import (
	"strings"
	"testing"
)

// The deterministic secret redactor: AWS access keys, PEM private-key
// blocks, scheme://user:pass@ credentials, password/secret/token
// assignments and high-entropy tokens of 32+ characters become
// [redacted] before any model call and before storage.

const pemBlock = "-----BEGIN RSA PRIVATE KEY-----\n" +
	"MIIEowIBAAKCAQEA7bq8m0Zl3c1cJp9vXv0yJ4k2m8r3cQ1sT5uV6wX7yZ8aB9cD\n" +
	"0eF1gH2iJ3kL4mN5oP6qR7sT8uV9wX0yZ1aB2cD3eF4gH5iJ6kL7mN8oP9qR0sT1\n" +
	"-----END RSA PRIVATE KEY-----"

func TestRedact_Secrets(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"aws access key", "key = AKIAIOSFODNN7EXAMPLE end", "key = [redacted] end"},
		{"aws session key", "id ASIAZZZZZZZZZZZZZZZZ.", "id [redacted]."},
		{"pem block", "before\n" + pemBlock + "\nafter", "before\n[redacted]\nafter"},
		{"pem in diff lines", "+x\n+-----BEGIN PRIVATE KEY-----\n+MIIEvQIBADANBgkqhkiG9w0B\n" +
			"+-----END PRIVATE KEY-----\n+y", "+x\n+[redacted]\n+y"},
		{"unterminated pem", "a\n-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIBl", "a\n[redacted]"},
		{"url credentials", "DATABASE_URL: postgres://app:s3cret@db.internal:5432/prod",
			"DATABASE_URL: postgres://[redacted]@db.internal:5432/prod"},
		{"url credentials mysql", `"mysql://root:hunter2@localhost/x"`,
			`"mysql://[redacted]@localhost/x"`},
		{"password assignment", `password = "hunter2"`, `password = "[redacted]"`},
		{"env assignment", "DB_PASSWORD=hunter2", "DB_PASSWORD=[redacted]"},
		{"yaml secret", "  client_secret: 'abc123xyz'", "  client_secret: '[redacted]'"},
		{"json token", `{"token": "eyJhbGciOiJIUzI1NiJ9"}`, `{"token": "[redacted]"}`},
		{"ruby hash", `secret => "s"`, `secret => "[redacted]"`},
		{"api key", "API_KEY := sk_live_51Habc", "API_KEY := [redacted]"},
		{"github token", "auth ghp_aB3dE5fG7hI9jK1lM3nO5pQ7rS9tU1vW3xY5 done",
			"auth [redacted] done"},
		{"mixed entropy token", "x 9fK2mQ7vX4pL8sT1wZ6nB3cR5yH0jD2gA4eU7 y", "x [redacted] y"},
	}
	for _, c := range cases {
		if got := Redact(c.in); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestRedact_LeavesCodeAndIdentifiersAlone(t *testing.T) {
	for _, in := range []string{
		"add_column :users, :password_digest, :string",
		"t.string :password",
		"    password_digest = models.CharField(max_length=128)",
		"  password: string;",
		`token = os.environ["TOKEN"]`,
		`password = ENV.fetch("DB_PASSWORD")`,
		"postgres://app@db.internal/prod",
		"test_migration_20261023_add_orders_index_for_status_lookup",
		"123e4567-e89b-12d3-a456-426614174000",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1",
		`password = ""`,
		"ALTER TABLE orders RENAME COLUMN email TO email_address;",
		"",
	} {
		if got := Redact(in); got != in {
			t.Errorf("Redact(%q) = %q, want it unchanged", in, got)
		}
	}
}

func TestRedact_Idempotent(t *testing.T) {
	in := "a AKIAIOSFODNN7EXAMPLE b " + pemBlock + " postgres://u:p@h/d password=\"x\" " +
		"ghp_aB3dE5fG7hI9jK1lM3nO5pQ7rS9tU1vW3xY5"
	once := Redact(in)
	if twice := Redact(once); twice != once {
		t.Fatalf("not idempotent:\n once %q\ntwice %q", once, twice)
	}
	for _, leak := range []string{"AKIAIOSFODNN7EXAMPLE", "MIIEowIBAAKCAQEA", "u:p@", `"x"`,
		"ghp_aB3"} {
		if strings.Contains(once, leak) {
			t.Errorf("%q leaked: %q", leak, once)
		}
	}
}

func FuzzRedact(f *testing.F) {
	f.Add("password = 'x' AKIAIOSFODNN7EXAMPLE postgres://a:b@c")
	f.Add(pemBlock)
	f.Add("CREATE ROLE r PASSWORD 'it''s' postgres://u:p/@q@h/d PGPASSWORD=a.1$")
	f.Fuzz(func(t *testing.T, in string) {
		out := Redact(in)
		if Redact(out) != out {
			t.Fatalf("not idempotent on %q", in)
		}
		if strings.Contains(out, "AKIAIOSFODNN7EXAMPLE") {
			t.Fatalf("access key leaked from %q", in)
		}
	})
}
