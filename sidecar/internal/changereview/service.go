package changereview

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// Stage time limits and the bound on writes after a run's context ended.
const (
	DefaultDeterministicTimeout = 20 * time.Second
	DefaultModelTimeout         = 75 * time.Second
	persistTimeout              = 5 * time.Second
	waitPoll                    = 200 * time.Millisecond
	zeroUUID                    = "00000000-0000-0000-0000-000000000000"
)

// Deps is what a database's Change Review service needs. Database, Pool,
// Config and Analyzer are required. Reasoner nil means no model (reviews
// complete with model_status no_model). HunkFilter returns the redacted
// data-access hunks of a diff (diffscan) for the content hash; nil hashes
// the whole diff, so any diff change breaks verdict carry-over. Redact
// scrubs secrets from every stored text. AutoClear computes the shadow
// would-auto-clear flag of a completed review (nil leaves it NULL). Go
// runs a review goroutine (the runtime's worker group; nil: go f()).
type Deps struct {
	Database   string
	Pool       *pgxpool.Pool
	Config     config.ChangeReviewConfig
	Analyzer   Analyzer
	Reasoner   Reasoner
	Notifier   Notifier
	HunkFilter func(diff string) string
	Redact     func(string) string
	AutoClear  func(Review) bool
	Render     RenderOptions
	Now        func() time.Time
	Go         func(func())
	Logf       func(format string, args ...any)

	DeterministicTimeout time.Duration
	ModelTimeout         time.Duration
}

// Service is Change Review for one monitored database: Submit queues a
// review on a bounded runner (max_concurrent_per_database running, 8
// queued, ErrBusy beyond), and the read and decision calls go to the
// store.
type Service struct {
	d      Deps
	store  *Store
	budget *Budget
	sem    chan struct{}

	mu      sync.Mutex
	ctx     context.Context
	pending int
	done    map[string]chan struct{}
}

// New builds the service; Start must run before Submit.
func New(d Deps) (*Service, error) {
	switch {
	case strings.TrimSpace(d.Database) == "":
		return nil, invalid("change review needs a database name")
	case d.Pool == nil:
		return nil, invalid("change review needs a connection pool")
	case d.Analyzer == nil:
		return nil, invalid("change review needs an analyzer")
	}
	if err := d.Config.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Go == nil {
		d.Go = func(f func()) { go f() }
	}
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	if d.DeterministicTimeout <= 0 {
		d.DeterministicTimeout = DefaultDeterministicTimeout
	}
	if d.ModelTimeout <= 0 {
		d.ModelTimeout = DefaultModelTimeout
	}
	return &Service{d: d, store: NewStore(d.Pool, d.Database, d.Now),
		budget: NewBudget(d.Pool, int64(d.Config.DailyTokensPerDatabase),
			int64(d.Config.DailyTokensPerActor), d.Now),
		sem:  make(chan struct{}, d.Config.MaxConcurrentPerDatabase),
		done: map[string]chan struct{}{}}, nil
}

// Start fails reviews a stopped process left running for more than
// InterruptAfter (reason interrupted) and accepts reviews until ctx ends.
// It returns how many reviews it interrupted.
func (s *Service) Start(ctx context.Context) (int, error) {
	n, err := s.store.InterruptStale(ctx, InterruptAfter)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.d.Logf("change review: %s: %d reviews left running were marked interrupted",
			s.d.Database, n)
	}
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	return n, nil
}

// Database is the service's database name.
func (s *Service) Database() string { return s.d.Database }

// Config is the service's configuration.
func (s *Service) Config() config.ChangeReviewConfig { return s.d.Config }

// Store is the service's store (the receipts job's transitions).
func (s *Service) Store() *Store { return s.store }

// Budget is the service's daily token budget (the reasoner reserves
// against it).
func (s *Service) Budget() *Budget { return s.budget }

// Get is one review as it stands.
func (s *Service) Get(ctx context.Context, id string) (Review, error) {
	return s.current(s.store.Get(ctx, id))
}

// current renders a complete review's comment from its stored state, so
// it shows what was recorded after completion: a DBA verdict (with its
// reason or edits), a landing, a decided receipt and the sealed marker.
func (s *Service) current(r Review, err error) (Review, error) {
	if err == nil && r.State == StateComplete {
		r.Markdown = RenderMarkdown(r, s.d.Render)
	}
	return r, err
}

// List is reviews matching f, newest first, without transcripts.
func (s *Service) List(ctx context.Context, f ListFilter) ([]Review, error) {
	return s.store.List(ctx, f)
}

// Decide records a DBA verdict (the surfaces allow it only to operator
// and admin sessions; no MCP tool calls it). The later revisions of the
// PR with the same content take it too (Store.Decide).
func (s *Service) Decide(ctx context.Context, id string, v DBAVerdict) (Review, error) {
	return s.current(s.store.Decide(ctx, id, v))
}

// Summary is the database's Reviews-page figures since.
func (s *Service) Summary(ctx context.Context, since time.Time) (Summary, error) {
	return s.store.Summary(ctx, since)
}

// Wait returns review id once it is no longer running, or after d (at
// most MaxWaitSeconds), whichever is first.
func (s *Service) Wait(ctx context.Context, id string, d time.Duration) (Review, error) {
	if d > MaxWaitSeconds*time.Second {
		d = MaxWaitSeconds * time.Second
	}
	r, err := s.store.Get(ctx, id)
	if err != nil || r.State != StateRunning || d <= 0 {
		return s.current(r, err)
	}
	s.mu.Lock()
	done := s.done[id]
	s.mu.Unlock()
	timer := time.NewTimer(d)
	defer timer.Stop()
	tick := time.NewTicker(waitPoll)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return s.current(s.store.Get(ctx, id))
		case <-timer.C:
			return s.current(s.store.Get(ctx, id))
		case <-ctx.Done():
			return r, ctx.Err()
		case <-tick.C:
			if r, err = s.store.Get(ctx, id); err != nil || r.State != StateRunning {
				return s.current(r, err)
			}
		}
	}
}
