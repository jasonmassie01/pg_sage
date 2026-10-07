package executor

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// missingGrant is one privilege the connected role lacks.
type missingGrant struct {
	what string // the privilege, e.g. "CREATE on schema public"
	need string // what pg_sage needs it for
	fix  string // the SQL that grants it
}

// schemaCreateGrant: CREATE INDEX and CREATE STATISTICS need CREATE on the
// table's schema, even for the table's owner.
func schemaCreateGrant(user string) missingGrant {
	return missingGrant{what: "CREATE on schema public",
		need: "CREATE INDEX and CREATE STATISTICS on its tables",
		fix:  "GRANT CREATE ON SCHEMA public TO " + user}
}

func signalBackendGrant(user string) missingGrant {
	return missingGrant{what: "pg_signal_backend",
		need: "cancelling a runaway query when an action needs it",
		fix:  "GRANT pg_signal_backend TO " + user}
}

// executesActions reports whether a trust level runs actions; only those
// levels need grants beyond reading the catalog and writing the sage schema.
func executesActions(level string) bool {
	return level == "advisory" || level == "autonomous"
}

// VerifyGrants checks the privileges autonomous execution uses. At a level
// that executes actions a missing grant is a warning with the SQL that
// fixes it; observation is read-only and needs none of them, so it only
// notes what higher levels would need (the "Grant more" guide).
func VerifyGrants(
	ctx context.Context,
	pool *pgxpool.Pool,
	user string,
	trustLevel string,
	logFn func(string, string, ...any),
) {
	// Resolve actual connected user (handles DATABASE_URL override).
	var actual string
	if err := pool.QueryRow(ctx, "SELECT current_user").Scan(&actual); err == nil {
		user = actual
	}
	var missing []missingGrant
	if checkSchemaCreate(ctx, pool, user, logFn) {
		missing = append(missing, schemaCreateGrant(user))
	}
	if checkSignalBackend(ctx, pool, user, logFn) {
		missing = append(missing, signalBackendGrant(user))
	}
	reportGrants(trustLevel, user, missing, logFn)
}

// reportGrants logs the missing grants for trustLevel: one warning each at
// a level that executes actions, one info line otherwise.
func reportGrants(
	trustLevel, user string, missing []missingGrant, logFn func(string, string, ...any),
) {
	if len(missing) == 0 {
		return
	}
	if !executesActions(trustLevel) {
		names := make([]string, len(missing))
		for i, m := range missing {
			names[i] = m.what
		}
		logFn("grants", "trust %s is read-only and needs no more grants; user %q lacks %s, "+
			"which higher trust levels use: see \"Grant more\" in Getting started",
			trustLevel, user, strings.Join(names, " and "))
		return
	}
	for _, m := range missing {
		logFn("grants", "WARNING: trust %s: user %q lacks %s, needed for %s; fix with: %s",
			trustLevel, user, m.what, m.need, m.fix)
	}
}

// checkSchemaCreate reports whether user lacks CREATE on schema public; a
// failed check is logged and reported as not missing.
func checkSchemaCreate(
	ctx context.Context,
	pool *pgxpool.Pool,
	user string,
	logFn func(string, string, ...any),
) bool {
	var hasCreate bool
	err := pool.QueryRow(ctx,
		"SELECT has_schema_privilege($1, 'public', 'CREATE')",
		user,
	).Scan(&hasCreate)
	if err != nil {
		logFn("grants",
			"could not check CREATE privilege on public schema: %v", err,
		)
		return false
	}
	return !hasCreate
}

// checkSignalBackend reports whether user is not a member of
// pg_signal_backend; a failed check is logged and reported as not missing.
func checkSignalBackend(
	ctx context.Context,
	pool *pgxpool.Pool,
	user string,
	logFn func(string, string, ...any),
) bool {
	var hasMembership bool
	err := pool.QueryRow(ctx,
		"SELECT pg_has_role($1, 'pg_signal_backend', 'MEMBER')",
		user,
	).Scan(&hasMembership)
	if err != nil {
		logFn("grants",
			"could not check pg_signal_backend membership: %v", err,
		)
		return false
	}
	return !hasMembership
}
