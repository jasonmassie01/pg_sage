package schema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"
)

// Migration ledger (config-04b). sage.schema_migrations records each step
// Bootstrap ran, by name and SHA-256 of its SQL, and the fingerprint of
// the sage catalog the last complete boot left (row ledgerCatalogID).
//
// When the catalog still has that fingerprint and no INVALID index,
// nothing changed the schema since: a boot skips every recorded step
// without running any of its SQL, so a restart issues no DDL. A step that
// validates a constraint still NOT VALID (legacy rows kept it so) runs on
// every boot until the constraint validates. When it differs (the first boot with the
// ledger, a boot cut off part way, an older pg_sage or an operator having
// changed the schema since, an INVALID index), the boot does not trust the
// records: every step runs through its config-04 catalog checks, as before
// the ledger, and is recorded; the new fingerprint is stored at the end.
// That first run is how the ledger is seeded from the catalog checks.
//
// A skipped step still runs its data statements (top-level DML, and DO
// blocks that only modify rows): rows an older pg_sage writes during a
// rolling upgrade or after a downgrade still converge, and a backfill cut
// short by its timeout resumes on the next start, as before the ledger.
//
// A recorded step whose SQL changed fails the boot, trusted or not: a
// released step must never change (the golden checksum test pins them); a
// new step is added instead.
const ddlSchemaMigrations = `
CREATE TABLE IF NOT EXISTS sage.schema_migrations (
    id              text PRIMARY KEY,
    checksum        text NOT NULL,
    applied_at      timestamptz NOT NULL DEFAULT now(),
    pg_sage_version text NOT NULL
);
`

// ledgerCatalogID is the ledger row holding the catalog fingerprint. The
// space keeps it apart from step names, which are Go identifiers.
const ledgerCatalogID = "catalog fingerprint"

// errMigrationChanged marks a recorded step whose SQL changed.
var errMigrationChanged = errors.New("migration step changed since it was applied")

// ledgerReadSQL reads the recorded steps, the current catalog fingerprint
// and the INVALID indexes in one round trip.
var ledgerReadSQL = `/* pg_sage migration ledger v1 */
SELECT COALESCE((SELECT array_agg(id ORDER BY id) FROM sage.schema_migrations), '{}'),
       COALESCE((SELECT array_agg(checksum ORDER BY id) FROM sage.schema_migrations), '{}'),
       (` + catalogFingerprintSQL + `),
       (` + catalogInvalidIndexSQL + `), (` + catalogNotValidSQL + `)`

const ledgerInsertSQL = `INSERT INTO sage.schema_migrations (id, checksum, pg_sage_version)
VALUES ($1, $2, $3) ON CONFLICT (id) DO NOTHING`

// ledgerSealSQL stores the fingerprint the boot left, writing only when it
// changed.
var ledgerSealSQL = `INSERT INTO sage.schema_migrations (id, checksum, pg_sage_version)
SELECT $1, (` + catalogFingerprintSQL + `), $2
ON CONFLICT (id) DO UPDATE
   SET checksum = EXCLUDED.checksum, applied_at = now(),
       pg_sage_version = EXCLUDED.pg_sage_version
 WHERE sage.schema_migrations.checksum IS DISTINCT FROM EXCLUDED.checksum`

// buildVersionValue is the pg_sage version recorded with each step.
var buildVersionValue atomic.Pointer[string]

// SetBuildVersion sets the pg_sage version the ledger records ("" resets
// it to "dev"). main calls it once at startup.
func SetBuildVersion(v string) {
	buildVersionValue.Store(&v)
}

func buildVersion() string {
	if v := buildVersionValue.Load(); v != nil && *v != "" {
		return *v
	}
	return "dev"
}

// stepChecksum is the SHA-256 hex digest of a step's SQL.
func stepChecksum(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(sum[:])
}

// migrationLedger is one bootstrap's view of sage.schema_migrations. It is
// loaded by the first step that runs and used by that session only.
type migrationLedger struct {
	loaded   bool
	trusted  bool              // the catalog is as the last complete boot left it
	ran      bool              // a step ran this boot: the catalog may have changed
	recorded map[string]string // step name -> checksum
	notValid []string          // NOT VALID sage constraints, "name sage.table"
}

type ledgerKey struct{}

// withMigrationLedger attaches a ledger to the bootstrap's context; the
// steps run under it consult and update it.
func withMigrationLedger(ctx context.Context, l *migrationLedger) context.Context {
	return context.WithValue(ctx, ledgerKey{}, l)
}

// ledgerFrom returns the context's ledger, or nil outside a bootstrap.
func ledgerFrom(ctx context.Context) *migrationLedger {
	l, _ := ctx.Value(ledgerKey{}).(*migrationLedger)
	return l
}

// load creates the ledger table when the catalog lacks it, then reads the
// recorded steps and compares the catalog with the recorded fingerprint.
// The sage schema must exist.
func (l *migrationLedger) load(ctx context.Context, db bootstrapDB) error {
	if l == nil || l.loaded {
		return nil
	}
	ledgerStep := migrationStep{name: "ddlSchemaMigrations", sql: ddlSchemaMigrations}
	if _, err := runCheckedStep(ctx, db, ledgerStep); err != nil {
		return fmt.Errorf("migration ledger: %w", err)
	}
	var ids, sums []string
	var fingerprint string
	var invalid []string
	if err := db.QueryRow(ctx, ledgerReadSQL).Scan(&ids, &sums, &fingerprint,
		&invalid, &l.notValid); err != nil {
		return fmt.Errorf("migration ledger: reading sage.schema_migrations: %w", err)
	}
	if len(ids) != len(sums) {
		return fmt.Errorf("migration ledger: %d ids, %d checksums", len(ids), len(sums))
	}
	l.recorded = make(map[string]string, len(ids))
	for i, id := range ids {
		l.recorded[id] = sums[i]
	}
	sealed, ok := l.recorded[ledgerCatalogID]
	l.trusted = ok && sealed == fingerprint && len(invalid) == 0
	if ok {
		for _, name := range invalid {
			slog.Warn(invalidIndexWarning(name))
		}
	}
	if ok && !l.trusted && len(invalid) == 0 {
		slog.Info("schema: the sage catalog changed since the last boot; " +
			"every migration step runs through its catalog checks")
	}
	l.loaded = true
	return nil
}

// noteRan marks that a step ran SQL this boot without being recorded (a
// deferred variant, config-04c), so the boot seals the catalog it left.
func (l *migrationLedger) noteRan() {
	if l != nil {
		l.ran = true
	}
}

// invalidIndexWarning is the WARN for an INVALID sage index outside the
// deferred list: only a step that builds the index rebuilds it.
func invalidIndexWarning(name string) string {
	for _, step := range migrationSteps() {
		if namesIdentifier(step.sql, name) {
			return fmt.Sprintf("schema: sage index %s is INVALID; every migration step "+
				"runs through its catalog checks, which rebuild it", name)
		}
	}
	return fmt.Sprintf("schema: sage index %s is INVALID; every migration step runs "+
		"through its catalog checks, but no step rebuilds this index; drop it "+
		"(DROP INDEX CONCURRENTLY sage.%s) or rebuild it (REINDEX INDEX CONCURRENTLY "+
		"sage.%s)", name, name, name)
}

// decide reports whether step may be skipped: it is recorded with its
// current checksum and the catalog is trusted. A recorded step with
// another checksum is an error wrapping errMigrationChanged.
func (l *migrationLedger) decide(step migrationStep) (bool, error) {
	if l == nil {
		return false, nil
	}
	recorded, ok := l.recorded[step.name]
	if !ok {
		return false, nil
	}
	if sum := stepChecksum(step.sql); recorded != sum {
		return false, fmt.Errorf("%w: %s was recorded with checksum %s, this build has %s. "+
			"A released step must not change; if this build is meant to re-apply it, "+
			"run DELETE FROM sage.schema_migrations WHERE id = '%s' and start again: "+
			"the step then runs through its catalog checks and is recorded anew",
			errMigrationChanged, step.name, recorded, sum, step.name)
	}
	if l.trusted && validatesPending(step.sql, l.notValid) {
		return false, nil // it validates a constraint still NOT VALID
	}
	return l.trusted, nil
}

// record writes step to the ledger once it ran or the catalog showed it
// done.
func (l *migrationLedger) record(ctx context.Context, db bootstrapDB, step migrationStep) error {
	if l == nil {
		return nil
	}
	l.ran = true
	sum := stepChecksum(step.sql)
	if l.recorded[step.name] == sum {
		return nil
	}
	if _, err := db.Exec(ctx, ledgerInsertSQL, step.name, sum, buildVersion()); err != nil {
		return fmt.Errorf("migration step %s: recording it in sage.schema_migrations: %w",
			step.name, err)
	}
	l.recorded[step.name] = sum
	return nil
}

// seal stores the catalog fingerprint a complete boot left, so the next
// boot can trust the records. A boot that skipped every step left the
// catalog as it found it and writes nothing.
func (l *migrationLedger) seal(ctx context.Context, db bootstrapDB) error {
	if l == nil || !l.loaded || (l.trusted && !l.ran) {
		return nil
	}
	if _, err := db.Exec(ctx, ledgerSealSQL, ledgerCatalogID, buildVersion()); err != nil {
		return fmt.Errorf("migration ledger: storing the catalog fingerprint: %w", err)
	}
	return nil
}

var (
	reDataHead = regexp.MustCompile(`(?is)^(?:(?:UPDATE|INSERT\s+INTO|DELETE\s+FROM)\s+` +
		`sage\.|WITH\s)`)
	reDOHead      = regexp.MustCompile(`(?is)^DO\s`)
	reDMLAnywhere = regexp.MustCompile(`(?i)\b(?:UPDATE|INSERT\s+INTO|DELETE\s+FROM)\s+sage\.`)
	// reNotDataKeyword is anything in a DO body that may change the catalog,
	// take a table lock or run unknown code.
	reNotDataKeyword = regexp.MustCompile(`(?i)\b(?:CREATE|ALTER|DROP|COMMENT|GRANT|` +
		`REVOKE|TRUNCATE|REINDEX|EXECUTE|PERFORM|LOCK|VACUUM|ANALYZE|CLUSTER|REFRESH)\b`)
)

// isDataStatement reports whether stmt only modifies rows: a top-level
// UPDATE, INSERT or DELETE on a sage table (or a WITH query), or a DO block
// that modifies sage rows and holds no keyword that could change the
// catalog.
func isDataStatement(stmt string) bool {
	head := strings.TrimSpace(stripSQLComments(stmt))
	if reDataHead.MatchString(head) {
		return true
	}
	return reDOHead.MatchString(head) && reDMLAnywhere.MatchString(head) &&
		!reNotDataKeyword.MatchString(head)
}

// dataStatements is step's SQL reduced to its settings and data
// statements, in order, or "" when it has no data statement.
func dataStatements(step migrationStep) string {
	var keep []string
	data := false
	for _, s := range classifyStep(step.sql) {
		switch {
		case s.shape == shapeSetting:
			keep = append(keep, s.sql)
		case isDataStatement(s.sql):
			keep = append(keep, s.sql)
			data = true
		}
	}
	if !data {
		return ""
	}
	return strings.Join(keep, ";")
}
