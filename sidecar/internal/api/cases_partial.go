package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// Per-source caps of one database's cases read. Each source reads one row
// past its cap so the response can say the list was cut (cases-05).
const (
	caseFindingsCap   = 500
	caseIncidentsCap  = 100
	caseQueryHintsCap = 200
)

// Error classes a database's cases read reports instead of raw error text,
// which can carry hosts, roles or SQL.
const (
	caseErrorTimeout    = "timeout"
	caseErrorConnection = "connection"
	caseErrorQuery      = "query"
)

// casesMaxConcurrentDatabases bounds how many databases one cases request
// reads at a time, so a large fleet does not take every pool at once.
const casesMaxConcurrentDatabases = 8

// casesDatabaseTimeout bounds one database's cases read, so a slow or
// unreachable database cannot hold the whole fleet view. A variable so
// tests can shorten it.
var casesDatabaseTimeout = 5 * time.Second

// readDatabaseCases is the per-database read; a seam for tests.
var readDatabaseCases = queryDatabaseCases

// casesTruncation says which of one database's sources had more rows than
// the cases view reads.
type casesTruncation struct {
	Findings   bool `json:"findings"`
	Incidents  bool `json:"incidents"`
	QueryHints bool `json:"query_hints"`
}

func (t casesTruncation) any() bool {
	return t.Findings || t.Incidents || t.QueryHints
}

// databaseCases is one database's projected cases and what was cut.
type databaseCases struct {
	cases     []cases.Case
	truncated casesTruncation
}

// casesUnavailable names a database whose cases could not be read.
type casesUnavailable struct {
	Database   string `json:"database"`
	ErrorClass string `json:"error_class"`
}

// casesReport is the cases of the selected databases, ranked globally,
// plus the databases that could not be read and the sources that were cut.
type casesReport struct {
	Cases       []cases.Case
	Unavailable []casesUnavailable
	Truncated   map[string]casesTruncation
	// errs holds each unavailable database's error, in Unavailable order,
	// for logs and callers; it never reaches a response.
	errs []error
}

// firstErr is the first database's read error, or nil.
func (r casesReport) firstErr() error {
	if len(r.errs) == 0 {
		return nil
	}
	return r.errs[0]
}

// collectCases reads every selected database's cases concurrently, each
// under its own deadline. A database that fails is reported unavailable
// and the others are still returned.
func collectCases(
	ctx context.Context, mgr *fleet.DatabaseManager, database string,
) casesReport {
	selected := poolsForDatabaseSelection(mgr, database)
	results := make([]databaseCases, len(selected))
	errs := make([]error, len(selected))
	slots := make(chan struct{}, casesMaxConcurrentDatabases)
	var wg sync.WaitGroup
	for i, sel := range selected {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			results[i], errs[i] = readDatabaseWithDeadline(ctx, mgr, sel)
		}()
	}
	wg.Wait()
	report := mergeDatabaseCases(selected, results, errs)
	addUnpooledDatabases(&report, mgr, database, selected)
	return report
}

// errNoDatabasePool is the read error of a selected database that has no
// pool, such as a fleet member that could not connect at startup.
var errNoDatabasePool = errors.New("database has no connection pool")

// addUnpooledDatabases names the selected databases that have no pool as
// unavailable (connection), so a database that was down when the sidecar
// started is reported instead of silently missing from the cases.
func addUnpooledDatabases(
	report *casesReport, mgr *fleet.DatabaseManager, database string,
	selected []namedPool,
) {
	read := make(map[string]bool, len(selected))
	for _, sel := range selected {
		read[sel.name] = true
	}
	var names []string
	for name, inst := range mgr.Instances() {
		if inst == nil || inst.Pool != nil || read[name] {
			continue
		}
		if database == "all" || name == database {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		slog.Warn("cases read skipped for database without a pool",
			"database", name, "error_class", caseErrorConnection)
		report.Unavailable = append(report.Unavailable,
			casesUnavailable{Database: name, ErrorClass: caseErrorConnection})
		report.errs = append(report.errs, errNoDatabasePool)
	}
}

func mergeDatabaseCases(
	selected []namedPool, results []databaseCases, errs []error,
) casesReport {
	report := emptyCasesReport()
	for i, sel := range selected {
		if errs[i] != nil {
			class := classifyCasesError(errs[i])
			slog.Warn("cases read failed for database",
				"database", sel.name, "error_class", class, "error", errs[i])
			report.Unavailable = append(report.Unavailable,
				casesUnavailable{Database: sel.name, ErrorClass: class})
			report.errs = append(report.errs, errs[i])
			continue
		}
		report.Cases = append(report.Cases, results[i].cases...)
		if results[i].truncated.any() {
			report.Truncated[sel.name] = results[i].truncated
		}
	}
	sortCasesGlobally(report.Cases)
	return report
}

// readDatabaseWithDeadline runs one database's read under
// casesDatabaseTimeout and returns by the deadline even if the read does
// not honour its context.
func readDatabaseWithDeadline(
	ctx context.Context, mgr *fleet.DatabaseManager, sel namedPool,
) (databaseCases, error) {
	dbCtx, cancel := context.WithTimeout(ctx, casesDatabaseTimeout)
	defer cancel()
	type outcome struct {
		res databaseCases
		err error
	}
	done := make(chan outcome, 1)
	// The read can outlive this call (it may ignore its context), so it is
	// handed the reader here rather than reading the package seam itself.
	read := readDatabaseCases
	go func() {
		res, err := read(dbCtx, mgr, sel)
		done <- outcome{res: res, err: err}
	}()
	select {
	case out := <-done:
		return out.res, out.err
	case <-dbCtx.Done():
		select {
		case out := <-done:
			return out.res, out.err
		default:
			return databaseCases{}, dbCtx.Err()
		}
	}
}

// classifyCasesError maps a read error to timeout, connection or query.
// A cancelled request counts as a timeout: the reader stopped waiting.
func classifyCasesError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) || pgconn.Timeout(err) {
		return caseErrorTimeout
	}
	var connectErr *pgconn.ConnectError
	var netErr net.Error
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &connectErr), errors.As(err, &netErr):
		return caseErrorConnection
	case errors.As(err, &pgErr):
		if len(pgErr.Code) == 5 && pgErr.Code[:2] == "08" {
			return caseErrorConnection
		}
		return caseErrorQuery
	case isConnectionError(err):
		return caseErrorConnection
	default:
		return caseErrorQuery
	}
}
