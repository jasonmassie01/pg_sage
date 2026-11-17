package doctor

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/startup"
)

// checkTimeout bounds every statement doctor sends; the CLI also sets
// statement_timeout and lock_timeout on its connection.
const checkTimeout = 5 * time.Second

// Skew limits: pass up to 2 s, warn up to 30 s, fail beyond.
const (
	skewWarnAbove = 2 * time.Second
	skewFailAbove = 30 * time.Second
)

// Check titles, shown next to each status.
const (
	titleConnection = "Connection"
	titleVersion    = "PostgreSQL version"
	titleRole       = "Role and grants"
	titleExtensions = "Extensions"
	titleProvider   = "Provider"
	titleQueryText  = "Query text visibility"
	titleClock      = "Clock skew"
)

// databaseChecks are the checks that read the target database.
type databaseChecks struct {
	connection, version, role, extensions, provider, queryText, clock Check
	platform                                                          Platform
}

// runDatabaseChecks runs the target's checks in order. After a failed
// connection every other one is unknown: one failure, not a cascade.
func runDatabaseChecks(ctx context.Context, in Inputs) databaseChecks {
	unknown := func(id, title string) Check { return unknownCheck(id, title, detailNoConnection) }
	db := databaseChecks{
		version:    unknown(IDVersion, titleVersion),
		role:       unknown(IDRoleGrants, titleRole),
		extensions: unknown(IDExtensions, titleExtensions),
		provider:   unknown(IDProvider, titleProvider),
		queryText:  unknown(IDQueryText, titleQueryText),
		clock:      unknown(IDClockSkew, titleClock),
		platform:   unknownPlatform,
	}
	var ok bool
	db.connection, ok = connectionCheck(ctx, in.Target)
	if !ok {
		return db
	}
	pool := in.Target
	num, text, err := readVersion(ctx, pool)
	db.version = versionCheck(num, text)
	if err != nil {
		db.version.Detail = "could not read the server version: " + err.Error()
	}
	db.platform = readPlatform(ctx, pool)
	db.role = roleCheck(ctx, pool)
	db.extensions = extensionsCheck(ctx, pool, db.platform)
	db.provider = providerCheck(db.platform)
	db.queryText = queryTextCheck(ctx, pool)
	db.clock = clockCheck(ctx, pool, in.Now)
	return db
}

// connectionCheck validates the endpoint and runs SELECT 1.
func connectionCheck(ctx context.Context, pool *pgxpool.Pool) (Check, bool) {
	c := Check{ID: IDConnection, Title: titleConnection, Status: StatusFail}
	if pool == nil {
		c.Detail = "no target database given"
		c.Fix = "pass --pg-url postgres://<role>@<host>:5432/<database>"
		return c, false
	}
	conn := pool.Config().ConnConfig
	if err := startup.ValidateSessionEndpoint(conn.Host, conn.Port); err != nil {
		c.Detail = err.Error()
		c.Fix = "connect to the provider's direct (session) endpoint"
		return c, false
	}
	qctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	var one int
	if err := pool.QueryRow(qctx, "/* pg_sage doctor */ SELECT 1").Scan(&one); err != nil {
		c.Detail = fmt.Sprintf("cannot connect to %s:%d as %s: %v", conn.Host, conn.Port,
			conn.User, err)
		c.Fix = "check the host, port, database, role and password, that the server " +
			"accepts connections from this host (pg_hba.conf, firewall or allow list) " +
			"and the sslmode the server requires"
		return c, false
	}
	c.Status = StatusPass
	c.Detail = fmt.Sprintf("connected to %s:%d/%s as %s", conn.Host, conn.Port,
		conn.Database, conn.User)
	return c, true
}

func readVersion(ctx context.Context, pool *pgxpool.Pool) (int, string, error) {
	qctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	var num int
	var text string
	err := pool.QueryRow(qctx, `/* pg_sage doctor */ SELECT
  pg_catalog.current_setting('server_version_num')::int,
  pg_catalog.current_setting('server_version')`).Scan(&num, &text)
	return num, text, err
}

// versionCheck classifies server_version_num: 14 to 18 pass, older fail,
// newer warn (not yet tested), zero unknown.
func versionCheck(num int, text string) Check {
	c := Check{ID: IDVersion, Title: titleVersion}
	switch {
	case num <= 0:
		c.Status = StatusUnknown
		c.Detail = "could not read the server version"
	case num < 140000:
		c.Status = StatusFail
		c.Detail = fmt.Sprintf("PostgreSQL %s: pg_sage needs PostgreSQL 14 or later", text)
		c.Fix = "upgrade to PostgreSQL 14 or later (17 is recommended)"
	case num >= 190000:
		c.Status = StatusWarn
		c.Detail = fmt.Sprintf("PostgreSQL %s is newer than the versions pg_sage is "+
			"tested with (14 to 18); report anything that misbehaves", text)
	default:
		c.Status = StatusPass
		c.Detail = "PostgreSQL " + text
	}
	return c
}

// clockCheck compares the server clock with the host clock, read on both
// sides of the query so the round trip does not count as skew.
func clockCheck(ctx context.Context, pool *pgxpool.Pool, now func() time.Time) Check {
	c := Check{ID: IDClockSkew, Title: titleClock}
	qctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	before := now()
	var server time.Time
	err := pool.QueryRow(qctx, "/* pg_sage doctor */ SELECT pg_catalog.clock_timestamp()").
		Scan(&server)
	after := now()
	if err != nil {
		c.Status = StatusUnknown
		c.Detail = "could not read the server clock: " + err.Error()
		return c
	}
	host := before.Add(after.Sub(before) / 2)
	skew := server.Sub(host)
	c.Status = skewStatus(skew)
	c.Detail = fmt.Sprintf("server clock is %s %s this host", absDuration(skew).
		Round(time.Millisecond), map[bool]string{true: "ahead of", false: "behind"}[skew >= 0])
	if c.Status != StatusPass {
		c.Fix = "sync both clocks with NTP (chrony or systemd-timesyncd); windows, " +
			"freshness and verification compare the two"
	}
	return c
}

// skewStatus is pass up to 2 s either way, warn up to 30 s, fail beyond.
func skewStatus(skew time.Duration) Status {
	d := absDuration(skew)
	switch {
	case d > skewFailAbove:
		return StatusFail
	case d > skewWarnAbove:
		return StatusWarn
	}
	return StatusPass
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
