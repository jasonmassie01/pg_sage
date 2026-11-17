package cloudtel

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"time"
)

// SupabaseDataAPITTL is how long one Data API read is used, and how long a
// failed read (a refused token, a rate limit) waits before the next one.
const SupabaseDataAPITTL = time.Hour

// supabaseDataAPITick is how often Run checks whether a read is due.
const supabaseDataAPITick = time.Minute

// supabaseDataAPICallTimeout bounds one Management API read.
const supabaseDataAPICallTimeout = 20 * time.Second

// DataAPIExposure is the project's Data API exposure as last read.
type DataAPIExposure struct {
	Schemas []string // the schemas the Data API serves
	ReadAt  time.Time
}

// SupabaseDataAPIOptions configure the reader; every field is optional.
type SupabaseDataAPIOptions struct {
	Now   func() time.Time
	Logf  func(format string, args ...any) // nil: nothing is logged
	After func(time.Duration) <-chan time.Time
}

// SupabaseDataAPI caches the project's Data API exposure: one read an
// hour, made by Run. Every failure is unknown (nil), never a stale or
// inferred "not served"; each kind of failure is logged once until a read
// succeeds.
type SupabaseDataAPI struct {
	api  SupabaseAPI
	opts SupabaseDataAPIOptions

	mu     sync.Mutex
	next   time.Time
	state  *DataAPIExposure
	logged map[error]bool
}

// NewSupabaseDataAPI is a reader over api; a nil api (no token) is always
// unknown and makes no call.
func NewSupabaseDataAPI(api SupabaseAPI, opts SupabaseDataAPIOptions) *SupabaseDataAPI {
	if api != nil {
		if v := reflect.ValueOf(api); v.Kind() == reflect.Pointer && v.IsNil() {
			api = nil
		}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.After == nil {
		opts.After = time.After
	}
	return &SupabaseDataAPI{api: api, opts: opts, logged: map[error]bool{}}
}

// Exposure is a copy of the last successful read, or nil when it is
// unknown: never read, no token, or the last read failed.
func (s *SupabaseDataAPI) Exposure() *DataAPIExposure {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == nil {
		return nil
	}
	return &DataAPIExposure{Schemas: slices.Clone(s.state.Schemas), ReadAt: s.state.ReadAt}
}

// Run refreshes at once, then checks every minute until ctx ends; a read
// is made only when the last one is an hour old.
func (s *SupabaseDataAPI) Run(ctx context.Context) {
	if s == nil || s.api == nil {
		return
	}
	for {
		s.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.opts.After(supabaseDataAPITick):
		}
	}
}

// Refresh reads the settings when the cached read (or the back-off after a
// failure) is an hour old. The lock is not held during the call, so a
// reader of Exposure never waits on the network; the next read time is
// claimed first, so concurrent refreshes make one call. An interrupted
// read (shutdown, or a cancel that is not the call's own timeout) is
// retried at the next refresh.
func (s *SupabaseDataAPI) Refresh(ctx context.Context) {
	if s == nil || s.api == nil {
		return
	}
	s.mu.Lock()
	now, prev := s.opts.Now(), s.next
	if now.Before(prev) {
		s.mu.Unlock()
		return
	}
	s.next = now.Add(SupabaseDataAPITTL)
	s.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, supabaseDataAPICallTimeout)
	settings, err := s.api.DataAPISettings(cctx)
	timedOut := errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil && !timedOut && (ctx.Err() != nil || errors.Is(err, context.Canceled)) {
		s.next = prev
		return
	}
	s.record(settings, err, now)
}

// record keeps a successful read, or unknown and the logged failure.
func (s *SupabaseDataAPI) record(settings SupabaseDataAPISettings, err error,
	now time.Time) {
	if err == nil && len(settings.Schemas) == 0 {
		err = fmt.Errorf("%w: data API settings list no schema", ErrSupabaseMalformed)
	}
	if err != nil {
		s.state = nil
		s.logOnce(err)
		return
	}
	s.state = &DataAPIExposure{Schemas: slices.Clone(settings.Schemas), ReadAt: now}
	clear(s.logged)
}

// logOnce logs err once per kind until a read succeeds.
func (s *SupabaseDataAPI) logOnce(err error) {
	kind := ErrSupabaseAPI
	for _, k := range []error{ErrSupabaseUnauthorized, ErrSupabaseRateLimited,
		ErrSupabaseMalformed} {
		if errors.Is(err, k) {
			kind = k
		}
	}
	if s.logged[kind] {
		return
	}
	s.logged[kind] = true
	s.opts.Logf("supabase data API exposure unknown (posture findings stay critical): %v",
		err)
}
