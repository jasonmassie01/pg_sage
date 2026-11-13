package schema

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// security-09b: sage.databases holds an optional action identity for a
// meta-DB member: a role name and its sealed password, both or neither,
// never the observation role.
//
// No concurrent access tests: the migration runs inside bootstrap, which
// is serialised by its advisory lock (covered by the bootstrap tests).

func TestDatabasesActionCredentialStepIsRegisteredAndBounded(t *testing.T) {
	var found []string
	for _, step := range migrationSteps() {
		if strings.Contains(step.sql, "action_password_enc") {
			found = append(found, step.name)
		}
	}
	if len(found) != 1 || found[0] != "ddlDatabasesActionCredential" {
		t.Fatalf("steps naming action_password_enc = %v, want only "+
			"ddlDatabasesActionCredential", found)
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"ADD COLUMN IF NOT EXISTS action_username TEXT",
		"ADD COLUMN IF NOT EXISTS action_password_enc BYTEA",
		"databases_action_credential_pair", "databases_action_role_distinct",
		"NOT VALID", "VALIDATE CONSTRAINT"} {
		if !strings.Contains(ddlDatabasesActionCredential, want) {
			t.Errorf("the step lacks %q:\n%s", want, ddlDatabasesActionCredential)
		}
	}
	tranche := trancheFourMigrations()
	if len(tranche) == 0 || tranche[0][0] != "ddlDatabasesActionCredential" {
		t.Fatalf("trancheFourMigrations = %v, want ddlDatabasesActionCredential first", tranche)
	}
}

// Integration: bootstrap runs twice cleanly on a fresh server; both
// constraints exist once and are validated, and the database enforces
// them on every write path.
func TestDatabasesActionCredentialBootstrapTwice(t *testing.T) {
	pool := freshPool(t, "databases_action_credential")
	ctx := t.Context()
	for run := 1; run <= 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	var n, validated int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE convalidated)
		FROM pg_constraint WHERE conrelid = 'sage.databases'::regclass
		AND conname IN ('databases_action_credential_pair',
		                'databases_action_role_distinct')`).Scan(&n, &validated); err != nil {
		t.Fatal(err)
	}
	if n != 2 || validated != 2 {
		t.Fatalf("%d action constraints (%d validated) after two runs, want 2 and 2",
			n, validated)
	}
	insert := `INSERT INTO sage.databases (name, host, database_name, username,
		password_enc, action_username, action_password_enc)
		VALUES ($1, 'h', 'd', 'observer', '\x00', $2, $3)`
	cases := []struct {
		name, user string
		enc        []byte
		constraint string
	}{
		{"username without password", "actor", nil, "databases_action_credential_pair"},
		{"password without username", "", []byte("x"), "databases_action_credential_pair"},
		{"observation role reused", "observer", []byte("x"), "databases_action_role_distinct"},
	}
	for _, tc := range cases {
		var user any
		if tc.user != "" {
			user = tc.user
		}
		_, err := pool.Exec(ctx, insert, "m-"+strings.ReplaceAll(tc.name, " ", "-"),
			user, tc.enc)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.ConstraintName != tc.constraint {
			t.Errorf("%s: insert error = %v, want a %s violation", tc.name, err, tc.constraint)
		}
	}
	if _, err := pool.Exec(ctx, insert, "ok-pair", "actor", []byte("x")); err != nil {
		t.Fatalf("a full pair on another role is refused: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, "ok-none", nil, nil); err != nil {
		t.Fatalf("a member without an action identity is refused: %v", err)
	}
}
