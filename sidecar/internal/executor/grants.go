package executor

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/rolegrants"
	"github.com/pg-sage/sidecar/internal/sqlident"
)

// missingGrant is one privilege the connected role lacks.
type missingGrant struct {
	what string // the privilege, e.g. "CREATE on schema app"
	need string // what pg_sage needs it for
	fix  string // the SQL that grants it
}

// schemaCreateGrant: CREATE INDEX and CREATE STATISTICS need CREATE on the
// table's schema, even for the table's owner. The wording and SQL come from
// rolegrants, which the Grant more guide uses too.
func schemaCreateGrant(user string, sc rolegrants.SchemaCreate) missingGrant {
	return missingGrant{what: sc.What(), need: rolegrants.Need,
		fix: sc.GrantSQL(sqlident.Role(user))}
}

// signalBackendGrant: cancelling another role's query needs the privileges
// of pg_signal_backend. A member that does not inherit them gets the SQL
// that fixes the inheritance, the same the Grant more guide shows.
func signalBackendGrant(user string, sig rolegrants.SignalBackend) missingGrant {
	what := "pg_signal_backend"
	if sig.MemberOnly {
		what += " (a member that does not inherit its privileges)"
	}
	return missingGrant{what: what, need: rolegrants.SignalNeed,
		fix: sig.GrantSQL(sqlident.Role(user))}
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
	// Resolve actual connected user (handles DATABASE_URL override) and
	// whether it is a superuser (the agent governance self-check, AP-14).
	var actual string
	var super bool
	if err := pool.QueryRow(ctx, selfRoleSQL).Scan(&actual, &super); err == nil {
		user = actual
	}
	var missing []missingGrant
	if sc, lacking := checkSchemaCreate(ctx, pool, logFn); lacking {
		missing = append(missing, schemaCreateGrant(user, sc))
	}
	if sig, lacking := checkSignalBackend(ctx, pool, logFn); lacking {
		missing = append(missing, signalBackendGrant(user, sig))
	}
	reportGrants(trustLevel, user, missing, logFn)
	reportSequences(ctx, pool, user, logFn)
	reportGuardRole(user, super, logFn)
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

// checkSchemaCreate reports whether the connected role lacks CREATE on a
// schema holding user tables; a failed check is logged and reported as not
// missing.
func checkSchemaCreate(
	ctx context.Context,
	pool *pgxpool.Pool,
	logFn func(string, string, ...any),
) (rolegrants.SchemaCreate, bool) {
	sc, err := rolegrants.CheckSchemaCreate(ctx, pool)
	if err != nil {
		logFn("grants",
			"could not check CREATE privilege on the schemas holding tables: %v", err,
		)
		return rolegrants.SchemaCreate{}, false
	}
	return sc, sc.Lacking()
}

// checkSignalBackend reports whether the connected role cannot use
// pg_signal_backend (rolegrants, which the Grant more guide uses too); a
// failed check is logged and reported as not missing.
func checkSignalBackend(
	ctx context.Context,
	pool *pgxpool.Pool,
	logFn func(string, string, ...any),
) (rolegrants.SignalBackend, bool) {
	sig, err := rolegrants.CheckSignalBackend(ctx, pool)
	if err != nil {
		logFn("grants", "could not check pg_signal_backend privileges: %v", err)
		return rolegrants.SignalBackend{}, false
	}
	return sig, !sig.Usable
}

// reportSequences notes the schemas whose sequences the connected role
// cannot read. The first look reads them at every trust level, so it is a
// note at every level, never a warning: without it the sequence runway
// check is degraded, not unsafe.
func reportSequences(
	ctx context.Context,
	pool *pgxpool.Pool,
	user string,
	logFn func(string, string, ...any),
) {
	sq, err := rolegrants.CheckSequenceSelect(ctx, pool)
	if err != nil {
		logFn("grants", "could not check SELECT on sequences: %v", err)
		return
	}
	if !sq.Lacking() {
		return
	}
	logFn("grants", "user %q lacks %s, needed for %s; fix with: %s",
		user, sq.What(), rolegrants.SequenceNeed, sq.GrantSQL(sqlident.Role(user)))
}
