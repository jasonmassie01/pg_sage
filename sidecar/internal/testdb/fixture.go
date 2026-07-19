// Package testdb owns the designated PostgreSQL test fixture boundary.
package testdb

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// EnvName is intentionally different from the production runtime DSN.
	EnvName       = "SAGE_TEST_DATABASE_URL"
	fixturePrefix = "pgsage_"
	disabledDSN   = "postgres://pgsage_test_disabled@127.0.0.1:1/" +
		"pgsage_test_disabled?sslmode=disable&connect_timeout=1"
)

var legacyDSNEnvNames = []string{
	"SAGE_DATABASE_URL",
	"SAGE_TEST_DSN",
	"PG_TEST_DSN",
	"PIPELINE_PG_URL",
	"HINT_TEST_DSN",
}

var fixtureNamePattern = regexp.MustCompile(`^pgsage_[a-z0-9_]+_[0-9]+_[a-f0-9]{8}$`)

// DesignatedDSN returns only the explicit test DSN. It never falls back to a
// production runtime URL or an implicit localhost database.
func DesignatedDSN() (string, error) {
	dsn := strings.TrimSpace(os.Getenv(EnvName))
	if dsn == "" {
		return "", fmt.Errorf("%s must name the disposable test PostgreSQL server", EnvName)
	}
	if _, err := pgx.ParseConfig(dsn); err != nil {
		return "", fmt.Errorf("parse %s: %w", EnvName, err)
	}
	return dsn, nil
}

// Run creates one database per package process, runs the package tests, then
// force-drops only that validated fixture database.
func Run(run func() int, packageID string) int {
	if strings.TrimSpace(os.Getenv(EnvName)) == "" {
		return runWithDSNEnvironment(run, disabledDSN)
	}
	baseDSN, err := DesignatedDSN()
	if err != nil {
		fmt.Fprintf(os.Stderr, "test fixture setup failed: %v\n", err)
		return 1
	}
	name := fixtureDatabaseName(packageID, os.Getpid())
	fixtureURL, err := createFixture(baseDSN, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "test fixture setup failed: %v\n", err)
		return 1
	}
	code := runWithDSNEnvironment(run, fixtureURL)
	if err := dropFixture(baseDSN, name); err != nil {
		fmt.Fprintf(os.Stderr, "test fixture cleanup failed: %v\n", err)
		return 1
	}
	return code
}

func runWithDSNEnvironment(run func() int, dsn string) int {
	names := append([]string{EnvName}, legacyDSNEnvNames...)
	previous := make(map[string]string, len(names))
	for _, name := range names {
		previous[name] = os.Getenv(name)
		if err := os.Setenv(name, dsn); err != nil {
			fmt.Fprintf(os.Stderr, "set test DSN environment: %v\n", err)
			return 1
		}
	}
	code := run()
	for _, name := range names {
		_ = os.Setenv(name, previous[name])
	}
	return code
}

func fixtureDatabaseName(packageID string, pid int) string {
	base := strings.ToLower(packageID)
	base = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(base, "_")
	base = strings.Trim(base, "_")
	if len(base) > 28 {
		base = base[len(base)-28:]
	}
	sum := sha256.Sum256([]byte(packageID))
	return fmt.Sprintf("%s%s_%d_%x", fixturePrefix, base, pid, sum[:4])
}

func fixtureDSN(baseDSN, database string) (string, error) {
	if err := validateFixtureDatabaseName(database); err != nil {
		return "", err
	}
	parsed, err := url.Parse(baseDSN)
	if err != nil {
		return "", fmt.Errorf("parse designated test DSN: %w", err)
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return "", errors.New("designated test DSN must be a postgres URL")
	}
	parsed.Path = "/" + database
	return parsed.String(), nil
}

func validateFixtureDatabaseName(database string) error {
	if !fixtureNamePattern.MatchString(database) || len(database) > 63 {
		return fmt.Errorf("refusing non-fixture database name %q", database)
	}
	return nil
}

func createFixture(baseDSN, database string) (string, error) {
	if err := validateFixtureDatabaseName(database); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		return "", fmt.Errorf("connect designated test server: %w", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	_, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()+
		" TEMPLATE template0")
	if err != nil {
		return "", fmt.Errorf("create fixture database %s: %w", database, err)
	}
	dsn, err := fixtureDSN(baseDSN, database)
	if err != nil {
		return "", errors.Join(err, dropFixture(baseDSN, database))
	}
	if err := installFixtureExtensions(ctx, dsn); err != nil {
		return "", errors.Join(err, dropFixture(baseDSN, database))
	}
	return dsn, nil
}

func installFixtureExtensions(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect fixture database: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	for _, extension := range []string{
		"pg_stat_statements", "pgcrypto", "pg_hint_plan", "vector",
	} {
		var available bool
		err := conn.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_available_extensions WHERE name=$1)`, extension).Scan(&available)
		if err != nil {
			return fmt.Errorf("check extension %s: %w", extension, err)
		}
		if available {
			_, err = conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS "+
				pgx.Identifier{extension}.Sanitize())
			if err != nil {
				return fmt.Errorf("create extension %s: %w", extension, err)
			}
		}
	}
	return nil
}

func dropFixture(baseDSN, database string) error {
	if err := validateFixtureDatabaseName(database); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		return fmt.Errorf("connect for fixture cleanup: %w", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	_, err = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+
		pgx.Identifier{database}.Sanitize()+" WITH (FORCE)")
	if err != nil {
		return fmt.Errorf("drop fixture database %s: %w", database, err)
	}
	return nil
}
