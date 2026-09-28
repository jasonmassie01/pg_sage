package value

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Source is one monitored database's value ledger (D3). The ledger lives in
// the monitored database next to the action it credits, so Name, the fleet
// instance name, is the only attribution label. sage.action_log.database_id
// holds the meta registry id when known and is not used for reads.
type Source struct {
	Name string
	Pool *pgxpool.Pool
}

// SourceResult is one database's read. A non-nil Err means the database's
// value is unknown for this read and the report is partial.
type SourceResult struct {
	Name     string
	Snapshot Snapshot
	Err      error
}

// SourceLister returns the current fleet. It is called once per read so
// databases added or removed at runtime are seen immediately.
type SourceLister func() []Source

var (
	ErrUnknownDatabase   = errors.New("unknown database")
	ErrSourceUnavailable = errors.New("database has no connection")
	ErrUnnamedSource     = errors.New("database has no instance name")
)

// UnnamedSourceLabel names a source registered without an instance name
// in Report.Unavailable; such a source is never credited under "".
const UnnamedSourceLabel = "(unnamed)"

const (
	maxConcurrentSourceReads = 8
	sourceReadTimeout        = 10 * time.Second
)

// FleetService reads the value ledger of every monitored database and
// merges it into one report. It serves standalone (a fleet of one), YAML
// fleet and meta-db mode alike; the meta database is never a source.
type FleetService struct{ list SourceLister }

func NewFleetService(list SourceLister) *FleetService {
	return &FleetService{list: list}
}

// Get returns the merged report. A database that cannot be read is named
// in Report.Unavailable and Report.Partial is set; it never fails the
// whole report. Filter.Database selects one instance ("" or "all" = all).
func (s *FleetService) Get(ctx context.Context, filter Filter) (Report, error) {
	results, err := s.Read(ctx, filter)
	if err != nil {
		return Report{}, err
	}
	return mergeResults(results), nil
}

// Read returns one result per selected database, in name order, with
// duplicates of the same physical database removed.
func (s *FleetService) Read(ctx context.Context, filter Filter) ([]SourceResult, error) {
	if s == nil || s.list == nil {
		return nil, ErrRepositoryUnavailable
	}
	sources, err := selectSources(s.list(), filter.Database)
	if err != nil {
		return nil, err
	}
	return readSources(ctx, sources, filter), nil
}

func selectSources(all []Source, database string) ([]Source, error) {
	sorted := append([]Source(nil), all...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Name < sorted[j].Name
	})
	if database == "" || database == "all" {
		return sorted, nil
	}
	for _, source := range sorted {
		if source.Name == database {
			return []Source{source}, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrUnknownDatabase, database)
}

type sourceRead struct {
	result   SourceResult
	identity string
}

func readSources(ctx context.Context, sources []Source, filter Filter) []SourceResult {
	reads := make([]sourceRead, len(sources))
	slots := make(chan struct{}, maxConcurrentSourceReads)
	var wg sync.WaitGroup
	for i, source := range sources {
		wg.Add(1)
		go func(i int, source Source) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			reads[i] = readSource(ctx, source, filter)
		}(i, source)
	}
	wg.Wait()
	return dedupeReads(reads)
}

func readSource(ctx context.Context, source Source, filter Filter) sourceRead {
	out := sourceRead{result: SourceResult{Name: source.Name}}
	if source.Name == "" {
		out.result.Name = UnnamedSourceLabel
		out.result.Err = ErrUnnamedSource
		return out
	}
	if source.Pool == nil {
		out.result.Err = fmt.Errorf("%w: %s", ErrSourceUnavailable, source.Name)
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, sourceReadTimeout)
	defer cancel()
	identity, err := physicalIdentity(ctx, source.Pool)
	if err == nil {
		out.identity = identity
		out.result.Snapshot, err = readSnapshot(ctx, source.Pool, source.Name, filter)
	}
	if err != nil {
		out.result.Snapshot = Snapshot{}
		out.result.Err = fmt.Errorf("read value snapshot for %s: %w", source.Name, err)
	}
	return out
}

// dedupeReads keeps the first name (reads are in name order) for each
// physical database, so two fleet entries for one database, or a meta
// database also registered as a target, are counted once.
func dedupeReads(reads []sourceRead) []SourceResult {
	seen := make(map[string]bool, len(reads))
	out := make([]SourceResult, 0, len(reads))
	for _, read := range reads {
		if read.result.Err == nil {
			if seen[read.identity] {
				continue
			}
			seen[read.identity] = true
		}
		out = append(out, read.result)
	}
	return out
}

// physicalIdentity names the database cluster and database behind a pool.
// When the role may not read pg_control_system(), the connection target
// (host:port/dbname) is used instead. Connection failures are returned.
func physicalIdentity(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var identity string
	err := pool.QueryRow(ctx, `SELECT system_identifier::text || '/' ||
		current_database() FROM pg_control_system()`).Scan(&identity)
	if err == nil {
		return identity, nil
	}
	var pgErr *pgconn.PgError
	if ctx.Err() != nil || !errors.As(err, &pgErr) {
		return "", err
	}
	conn := pool.Config().ConnConfig
	return fmt.Sprintf("%s:%d/%s", conn.Host, conn.Port, conn.Database), nil
}

// mergeResults sums every readable database into one report and names the
// ones that could not be read.
func mergeResults(results []SourceResult) Report {
	merged := Snapshot{ByFeatureMinutes: map[string]float64{}}
	byDB := map[string]float64{}
	byDay := map[string]float64{}
	unavailable := []string{}
	for _, result := range results {
		if result.Err != nil {
			unavailable = append(unavailable, result.Name)
			continue
		}
		addSnapshot(&merged, result.Snapshot, byDB, byDay)
	}
	merged.ByDatabaseMinutes = databaseRows(byDB)
	merged.TrendMinutes = dayRows(byDay)
	sort.SliceStable(merged.Incidents, func(i, j int) bool {
		return merged.Incidents[i].OccurredAt.Before(merged.Incidents[j].OccurredAt)
	})
	sort.Strings(unavailable)
	report := reportFromSnapshot(merged)
	report.Partial = len(unavailable) > 0
	report.Unavailable = unavailable
	return report
}

func addSnapshot(
	merged *Snapshot, snapshot Snapshot, byDB, byDay map[string]float64,
) {
	merged.AllTimeMinutes += snapshot.AllTimeMinutes
	merged.MonthMinutes += snapshot.MonthMinutes
	merged.WeekMinutes += snapshot.WeekMinutes
	merged.PotentialMinutes += snapshot.PotentialMinutes
	merged.IncidentMinutes += snapshot.IncidentMinutes
	merged.Incidents = append(merged.Incidents, snapshot.Incidents...)
	for feature, minutes := range snapshot.ByFeatureMinutes {
		merged.ByFeatureMinutes[feature] += minutes
	}
	for _, row := range snapshot.ByDatabaseMinutes {
		byDB[row.Name] += row.Minutes
	}
	for _, row := range snapshot.TrendMinutes {
		byDay[row.Day] += row.Minutes
	}
}
