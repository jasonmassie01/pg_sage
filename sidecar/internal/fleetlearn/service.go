package fleetlearn

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DatabaseSource is one fleet database the cycle reads.
type DatabaseSource struct {
	Name     string
	Boundary string
	Pool     *pgxpool.Pool
}

// Settings tune the service; zero values take the defaults.
type Settings struct {
	IncludeNames     bool
	MinSimilarity    float64 // 0: DefaultMinSimilarity
	MinPriorOutcomes int     // 0: DefaultMinPriorOutcomes
	WindowDays       int     // 0: DefaultWindowDays
	MaxTables        int
	MaxQueries       int
}

// Defaults.
const (
	DefaultMinSimilarity    = 0.6
	DefaultMinPriorOutcomes = 3
	DefaultWindowDays       = 90
	DefaultCacheTTL         = 5 * time.Minute
	databaseReadTimeout     = 60 * time.Second
)

// ErrDatabaseRequired is returned when no database is named.
var ErrDatabaseRequired = errors.New("fleetlearn: database required")

// Service runs the fingerprint cycle and answers look-alike and prior
// questions for one fleet scope.
type Service struct {
	store   *Store
	sources func() []DatabaseSource
	set     Settings
	logFn   func(string, string, ...any)
	now     func() time.Time

	mu       sync.Mutex
	cache    []Fingerprint
	cachedAt time.Time
	cached   bool
}

// NewService returns the service; sources lists the fleet's databases.
func NewService(store *Store, sources func() []DatabaseSource, s Settings,
	logFn func(string, string, ...any)) *Service {
	if s.MinSimilarity <= 0 {
		s.MinSimilarity = DefaultMinSimilarity
	}
	if s.MinPriorOutcomes <= 0 {
		s.MinPriorOutcomes = DefaultMinPriorOutcomes
	}
	if s.WindowDays <= 0 {
		s.WindowDays = DefaultWindowDays
	}
	if logFn == nil {
		logFn = func(string, string, ...any) {}
	}
	if sources == nil {
		sources = func() []DatabaseSource { return nil }
	}
	return &Service{store: store, sources: sources, set: s, logFn: logFn, now: time.Now}
}

// CycleResult summarizes one fingerprint cycle.
type CycleResult struct {
	Databases int      `json:"databases"`
	Failed    []string `json:"failed"`
}

// RunCycle fingerprints every fleet database, records its outcome digest
// and drops departed databases. A database that cannot be read is logged
// and skipped; a lost lease (ErrFenced) stops the cycle at once.
func (s *Service) RunCycle(ctx context.Context, f Fence) (CycleResult, error) {
	res := CycleResult{Failed: []string{}}
	srcs := s.sources()
	names := make([]string, 0, len(srcs))
	for _, src := range srcs {
		names = append(names, src.Name)
		err := s.learnOne(ctx, f, src)
		if errors.Is(err, ErrFenced) {
			return res, err
		}
		if err != nil {
			s.logFn("WARN", "fleet learning: database %s skipped this cycle: %v",
				src.Name, err)
			res.Failed = append(res.Failed, src.Name)
			continue
		}
		res.Databases++
	}
	if err := s.store.Prune(ctx, f, names); err != nil {
		return res, err
	}
	s.invalidate()
	return res, nil
}

func (s *Service) learnOne(ctx context.Context, f Fence, src DatabaseSource) error {
	rctx, cancel := context.WithTimeout(ctx, databaseReadTimeout)
	defer cancel()
	fp, idx, err := ReadFingerprint(rctx, src.Pool, ReadOptions{Database: src.Name,
		Boundary: src.Boundary, IncludeNames: s.set.IncludeNames,
		MaxTables: s.set.MaxTables, MaxQueries: s.set.MaxQueries})
	if err != nil {
		return err
	}
	since := s.now().Add(-time.Duration(s.set.WindowDays) * 24 * time.Hour)
	counts, err := ReadOutcomeDigest(rctx, src.Pool, idx, since)
	if err != nil {
		return err
	}
	if err := s.store.SaveFingerprint(ctx, f, fp); err != nil {
		return err
	}
	return s.store.SaveDigest(ctx, f, src.Name, counts)
}

func (s *Service) invalidate() {
	s.mu.Lock()
	s.cached = false
	s.mu.Unlock()
}

// fingerprints are the scope's fingerprints, cached for DefaultCacheTTL so
// per-proposal prior lookups do not each read the control database.
func (s *Service) fingerprints(ctx context.Context) ([]Fingerprint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached && s.now().Sub(s.cachedAt) < DefaultCacheTTL {
		return s.cache, nil
	}
	fps, err := s.store.Fingerprints(ctx)
	if err != nil {
		return nil, err
	}
	s.cache, s.cachedAt, s.cached = fps, s.now(), true
	return fps, nil
}

// LookAlikes are database's look-alikes; none before its first
// fingerprint.
func (s *Service) LookAlikes(ctx context.Context, database string) ([]LookAlike, error) {
	if database == "" {
		return nil, ErrDatabaseRequired
	}
	fps, err := s.fingerprints(ctx)
	if err != nil {
		return nil, err
	}
	for _, fp := range fps {
		if fp.Database == database {
			return LookAlikes(fp, fps, s.set.MinSimilarity), nil
		}
	}
	return []LookAlike{}, nil
}

// Prior is the look-alike evidence for an action of class on tables of
// database (the first table's shape is matched); ok is false when there
// is not enough of it.
func (s *Service) Prior(ctx context.Context, database, class string,
	tables []string) (Prior, bool, error) {
	if database == "" {
		return Prior{}, false, ErrDatabaseRequired
	}
	looks, err := s.LookAlikes(ctx, database)
	if err != nil || len(looks) == 0 {
		return Prior{}, false, err
	}
	shape, err := s.targetShape(ctx, database, tables)
	if err != nil {
		return Prior{}, false, err
	}
	names := make([]string, len(looks))
	for i, l := range looks {
		names[i] = l.Database
	}
	digests, err := s.store.Digests(ctx, names, class)
	if err != nil {
		return Prior{}, false, err
	}
	p, ok := BuildPrior(digests, looks, class, shape, s.set.MinPriorOutcomes)
	return p, ok, nil
}

func (s *Service) targetShape(ctx context.Context, database string,
	tables []string) (string, error) {
	if len(tables) == 0 {
		return "", nil
	}
	for _, src := range s.sources() {
		if src.Name == database {
			shape, err := ReadTableShape(ctx, src.Pool, tables[0])
			if err != nil {
				return "", fmt.Errorf("fleetlearn: shape of %s on %s: %w", tables[0],
					database, err)
			}
			return shape, nil
		}
	}
	return "", nil
}
