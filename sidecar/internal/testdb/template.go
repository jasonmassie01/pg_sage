package testdb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clusterlock"
)

// ci-15: a package that opts in (RunBootstrapped) copies its fixture from
// one template database per test run, bootstrapped once, instead of every
// package bootstrapping an empty database at the same moment.
const (
	templatePrefix = "pgsage_tpl_"
	// templateLock is the cluster lock the builder holds; the other
	// packages wait on it, then reuse the template.
	templateLock = "template"
	// templateBuildTimeout bounds the wait for the lock plus one build.
	templateBuildTimeout = 10 * time.Minute
	// templateSweepAge is how old a template of another hash must be
	// before the sweep drops it: a run of another build may still use it.
	templateSweepAge = time.Hour
	// checksumFile and schemaDir are the template hash's inputs, relative
	// to the module root.
	checksumFile = "internal/schema/testdata/migration_checksums.txt"
	schemaDir    = "internal/schema"
	// templateStampPrefix starts the comment that dates a template.
	templateStampPrefix = "pg_sage test template built "
)

var templateNamePattern = regexp.MustCompile(`^pgsage_tpl_[a-f0-9]{16}$`)

// errNoChecksums means the template hash has no migration checksum file
// to start from: the fixtures start from template0.
var errNoChecksums = errors.New("no migration checksum file")

// BootstrapFunc bootstraps a database: schema.Bootstrap (testdb cannot
// import schema, whose tests import testdb).
type BootstrapFunc func(context.Context, *pgxpool.Pool) error

func validateTemplateDatabaseName(database string) error {
	if !templateNamePattern.MatchString(database) {
		return fmt.Errorf("refusing non-template database name %q", database)
	}
	return nil
}

// validateFixtureTemplate accepts template0 or one of our templates.
func validateFixtureTemplate(template string) error {
	if template == "template0" {
		return nil
	}
	return validateTemplateDatabaseName(template)
}

// moduleRoot is the nearest directory at or above the working directory
// holding go.mod (a test binary runs in its package's directory).
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod at or above the working directory")
		}
		dir = parent
	}
}

// templateHash names the schema a template holds: the migration checksum
// file plus every non-test Go file of the schema package, so any schema
// code change makes a new template. It is errNoChecksums without a
// non-empty checksum file.
func templateHash(root string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("%w: no module root", errNoChecksums)
	}
	sums, err := os.ReadFile(filepath.Join(root, checksumFile))
	if err != nil || len(sums) == 0 {
		return "", fmt.Errorf("%w: %s (%v)", errNoChecksums, checksumFile, err)
	}
	sources, err := filepath.Glob(filepath.Join(root, schemaDir, "*.go"))
	if err != nil {
		return "", fmt.Errorf("list %s: %w", schemaDir, err)
	}
	sort.Strings(sources)
	h := sha256.New()
	h.Write(sums)
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", path, err)
		}
		_, _ = fmt.Fprintf(h, "\x00%s\x00%d\x00", filepath.Base(path), len(body))
		h.Write(body)
	}
	return hex.EncodeToString(h.Sum(nil)[:8]), nil
}

// ensureTemplate makes name a ready template, building it with bootstrap
// unless a ready one exists. Callers across processes serialize on the
// template cluster lock, so one builds and the others reuse it. A build
// that fails leaves nothing behind and returns the bootstrap's error.
func ensureTemplate(ctx context.Context, baseDSN, name string, bootstrap BootstrapFunc) error {
	if err := validateTemplateDatabaseName(name); err != nil {
		return err
	}
	release, err := clusterlock.Lock(ctx, baseDSN, templateLock)
	if err != nil {
		return fmt.Errorf("fixture template %s: wait for the template lock: %w", name, err)
	}
	defer release()
	admin, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		return fmt.Errorf("fixture template %s: connect designated test server: %w", name, err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	exists, ready, err := templateState(ctx, admin, name)
	if err != nil {
		return fmt.Errorf("fixture template %s: %w", name, err)
	}
	if !ready {
		if exists { // a builder died before marking it
			if err := dropTemplate(ctx, admin, name); err != nil {
				return fmt.Errorf("fixture template %s: drop the half-built copy: %w", name, err)
			}
		}
		if err := buildTemplate(ctx, admin, baseDSN, name, bootstrap); err != nil {
			return errors.Join(fmt.Errorf("build fixture template %s: %w", name, err),
				dropTemplate(context.WithoutCancel(ctx), admin, name))
		}
	}
	dropped, err := sweepTemplates(ctx, admin, name, time.Now().Add(-templateSweepAge))
	if err != nil || len(dropped) > 0 {
		_, _ = fmt.Fprintf(diag(), "testdb: swept fixture templates %v (%v)\n", dropped, err)
	}
	return nil
}

func templateState(ctx context.Context, admin *pgx.Conn, name string) (bool, bool, error) {
	var exists, ready bool
	err := admin.QueryRow(ctx, `SELECT count(*) > 0, COALESCE(bool_and(datistemplate), false)
		FROM pg_database WHERE datname = $1`, name).Scan(&exists, &ready)
	if err != nil {
		return false, false, fmt.Errorf("read pg_database: %w", err)
	}
	return exists, ready, nil
}

// buildTemplate creates name from template0, bootstraps it, closes every
// session, then marks it a template nobody can connect to: CREATE
// DATABASE ... TEMPLATE refuses a source with sessions.
func buildTemplate(
	ctx context.Context, admin *pgx.Conn, baseDSN, name string, bootstrap BootstrapFunc,
) error {
	if bootstrap == nil {
		return errors.New("no bootstrap function")
	}
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident+" TEMPLATE template0"); err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	dsn, err := templateDSN(baseDSN, name)
	if err != nil {
		return err
	}
	if err := installFixtureExtensions(ctx, dsn); err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	err = bootstrap(ctx, pool)
	pool.Close()
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if err := stampTemplate(ctx, admin, name, time.Now()); err != nil {
		return err
	}
	if _, err := admin.Exec(ctx, "ALTER DATABASE "+ident+
		" WITH ALLOW_CONNECTIONS false IS_TEMPLATE true"); err != nil {
		return fmt.Errorf("mark as template: %w", err)
	}
	return waitNoSessions(ctx, admin, name)
}

// stampTemplate dates name for the sweep. COMMENT takes no parameters; the
// text is a fixed prefix and an RFC 3339 time.
func stampTemplate(ctx context.Context, admin *pgx.Conn, name string, at time.Time) error {
	if err := validateTemplateDatabaseName(name); err != nil {
		return err
	}
	_, err := admin.Exec(ctx, "COMMENT ON DATABASE "+pgx.Identifier{name}.Sanitize()+
		" IS '"+templateStampPrefix+at.UTC().Format(time.RFC3339)+"'")
	if err != nil {
		return fmt.Errorf("date template: %w", err)
	}
	return nil
}

// waitNoSessions waits until the bootstrap's closed sessions have left
// name.
func waitNoSessions(ctx context.Context, admin *pgx.Conn, name string) error {
	for {
		var n int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = $1`, name).Scan(&n); err != nil {
			return fmt.Errorf("count sessions: %w", err)
		}
		if n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%d sessions still in the template: %w", n, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// dropTemplate drops name if it exists, template or not.
func dropTemplate(ctx context.Context, admin *pgx.Conn, name string) error {
	if err := validateTemplateDatabaseName(name); err != nil {
		return err
	}
	exists, _, err := templateState(ctx, admin, name)
	if err != nil || !exists {
		return err
	}
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "ALTER DATABASE "+ident+" WITH IS_TEMPLATE false"); err != nil {
		return fmt.Errorf("unmark template %s: %w", name, err)
	}
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop template %s: %w", name, err)
	}
	return nil
}

// sweepTemplates drops the templates other than keep that were built
// before cutoff (or carry no date) and returns their names.
func sweepTemplates(
	ctx context.Context, admin *pgx.Conn, keep string, cutoff time.Time,
) ([]string, error) {
	rows, err := admin.Query(ctx, `SELECT datname,
		COALESCE(pg_catalog.shobj_description(oid, 'pg_database'), '')
		FROM pg_database WHERE datname LIKE 'pgsage\_tpl\_%' AND datname <> $1
		ORDER BY datname`, keep)
	if err != nil {
		return nil, fmt.Errorf("list fixture templates: %w", err)
	}
	type tpl struct{ name, comment string }
	found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (tpl, error) {
		var t tpl
		return t, r.Scan(&t.name, &t.comment)
	})
	if err != nil {
		return nil, fmt.Errorf("list fixture templates: %w", err)
	}
	var dropped []string
	var errs []error
	for _, t := range found {
		if validateTemplateDatabaseName(t.name) != nil || builtAfter(t.comment, cutoff) {
			continue
		}
		if err := dropTemplate(ctx, admin, t.name); err != nil {
			errs = append(errs, err)
			continue
		}
		dropped = append(dropped, t.name)
	}
	return dropped, errors.Join(errs...)
}

// builtAfter reports whether a template's date stamp is after cutoff.
func builtAfter(comment string, cutoff time.Time) bool {
	stamp, ok := strings.CutPrefix(comment, templateStampPrefix)
	if !ok {
		return false
	}
	built, err := time.Parse(time.RFC3339, stamp)
	return err == nil && built.After(cutoff)
}
