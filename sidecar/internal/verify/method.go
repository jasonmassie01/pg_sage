package verify

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// Verification v2's switch (lab-01c). verify.method v1 judges by v1 only;
// shadow computes v2 beside v1 and acts on v1; v2 acts on v2 and keeps v1
// for comparison; auto is v2 on a database whose latest verifier
// back-test (lab-02) passed and is at most BacktestMaxAge old, shadow
// otherwise. Without a back-test auto is shadow, so the switch changes no
// verdict on its own.
const (
	MethodV1     = "v1"
	MethodShadow = "shadow"
	MethodV2     = "v2"
	MethodAuto   = "auto"
)

// Verifiers name which judge decided a verdict (sage.action_outcome.verifier).
const (
	VerifierV1 = "v1"
	VerifierV2 = "v2"
)

// BacktestMaxAge is how old a passing back-test may be for auto to act on
// v2; MethodCacheTTL how long a resolution is reused before the back-test
// is read again (a new back-test takes effect without a restart).
const (
	BacktestMaxAge = 35 * 24 * time.Hour
	MethodCacheTTL = 10 * time.Minute
)

// methods are the configurable values (config.VerifyMethods).
var methods = []string{MethodV1, MethodShadow, MethodV2, MethodAuto}

// Backtest is a database's latest verifier back-test.
type Backtest struct {
	Passed bool
	RanAt  time.Time
}

// BacktestReader reads a database's latest back-test; ok is false when
// none exists.
type BacktestReader interface {
	LatestBacktest(ctx context.Context, database string) (Backtest, bool, error)
}

// NoBacktest reports no back-test for every database. It is the reader
// until lab-02 creates sage.verifier_backtest, so auto never reads a
// table that does not exist.
type NoBacktest struct{}

// LatestBacktest reports none.
func (NoBacktest) LatestBacktest(context.Context, string) (Backtest, bool, error) {
	return Backtest{}, false, nil
}

// Resolution is the verifier a database's watches use now, and why.
type Resolution struct {
	Method string
	Reason string
}

type cachedResolution struct {
	resolution Resolution
	at         time.Time
}

// MethodResolver resolves verify.method per database, caching auto's
// back-test read for MethodCacheTTL. Safe for concurrent use.
type MethodResolver struct {
	reader BacktestReader
	now    func() time.Time
	mu     sync.Mutex
	cache  map[string]cachedResolution
}

// NewMethodResolver resolves over reader (NoBacktest when nil).
func NewMethodResolver(reader BacktestReader, now func() time.Time) *MethodResolver {
	if reader == nil {
		reader = NoBacktest{}
	}
	if now == nil {
		now = time.Now
	}
	return &MethodResolver{reader: reader, now: now, cache: map[string]cachedResolution{}}
}

// Resolve is the method configured resolves to on database. An unknown
// configured value is refused and resolves to v1 (today's verifier); a
// back-test that cannot be read resolves to shadow, is returned as the
// error and is not cached.
func (r *MethodResolver) Resolve(ctx context.Context, configured, database string) (
	Resolution, error) {
	if !slices.Contains(methods, configured) {
		return Resolution{Method: MethodV1, Reason: "unknown verify.method"},
			fmt.Errorf("verify.method %q is not one of %s: judging by v1", configured,
				strings.Join(methods, ", "))
	}
	if configured != MethodAuto {
		return Resolution{Method: configured, Reason: "verify.method " + configured}, nil
	}
	if r == nil {
		return Resolution{Method: MethodShadow, Reason: "no back-test reader"}, nil
	}
	now := r.now()
	r.mu.Lock()
	cached, ok := r.cache[database]
	r.mu.Unlock()
	if ok && now.Sub(cached.at) < MethodCacheTTL {
		return cached.resolution, nil
	}
	b, found, err := r.reader.LatestBacktest(ctx, database)
	if err != nil {
		return Resolution{Method: MethodShadow, Reason: "back-test unreadable"},
			fmt.Errorf("read the verifier back-test of database %q: %w", database, err)
	}
	res := autoResolution(b, found, now)
	r.mu.Lock()
	r.cache[database] = cachedResolution{resolution: res, at: now}
	r.mu.Unlock()
	return res, nil
}

// autoResolution is auto's choice for the latest back-test.
func autoResolution(b Backtest, found bool, now time.Time) Resolution {
	switch {
	case !found:
		return Resolution{Method: MethodShadow, Reason: "no back-test yet"}
	case !b.Passed:
		return Resolution{Method: MethodShadow, Reason: "the latest back-test failed"}
	case b.RanAt.IsZero() || b.RanAt.After(now):
		return Resolution{Method: MethodShadow, Reason: "the latest back-test has no " +
			"valid run time"}
	case now.Sub(b.RanAt) > BacktestMaxAge:
		return Resolution{Method: MethodShadow, Reason: fmt.Sprintf("the latest passing "+
			"back-test is older than %d days", int(BacktestMaxAge.Hours()/24))}
	}
	return Resolution{Method: MethodV2, Reason: "a passing back-test from " +
		b.RanAt.UTC().Format(time.RFC3339)}
}
