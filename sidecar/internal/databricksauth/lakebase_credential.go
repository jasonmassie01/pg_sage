package databricksauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/databricks/databricks-sdk-go/apierr"
	"github.com/databricks/databricks-sdk-go/service/postgres"
)

// Refresh timing for minted database credentials. Lakebase checks a token's
// expiry only at login, so an established connection outlives its token and
// only new physical connections need a valid one. Variables so tests can
// shrink them.
var (
	// refreshAhead: a cached token with more than this left is used as is.
	refreshAhead = 15 * time.Minute
	// minUsable: when a mint fails, a cached token with more than this left
	// still authorizes the connection; below it the connection is refused.
	minUsable = 30 * time.Second
	// mintTimeout bounds one mint, so a control-plane brownout (the SDK
	// retries 429/503 for minutes) cannot stall pool growth.
	mintTimeout = 20 * time.Second
	// fallbackWarnEvery rate-limits the warning for serving a cached token.
	fallbackWarnEvery = time.Minute
	credentialWarn    = slog.Warn
)

var errBadCredential = errors.New("lakebase returned an empty or expired database credential")

// CredentialState is a read-only snapshot of an endpoint's database
// credential refresh. It never contains a token. LastError is the mint error
// text for logs; status projections use LastErrorClass instead.
type CredentialState struct {
	LastMintAt          time.Time
	ExpiresAt           time.Time
	ConsecutiveFailures int
	LastError           string
	LastErrorClass      string
}

// EndpointCredential is one tracked endpoint's credential refresh state.
type EndpointCredential struct {
	Endpoint string
	State    CredentialState
}

var credentialStates sync.Map // endpoint resource path -> CredentialState

// CredentialStatus reports the latest refresh outcome for endpoint from any
// pool using it. ok is false before the first mint attempt.
func CredentialStatus(endpoint string) (state CredentialState, ok bool) {
	v, ok := credentialStates.Load(endpoint)
	if !ok {
		return CredentialState{}, false
	}
	return v.(CredentialState), true
}

// CredentialStatuses lists every tracked endpoint's latest refresh outcome,
// sorted by endpoint. It is empty before any tracked pool attempts a mint.
func CredentialStatuses() []EndpointCredential {
	out := []EndpointCredential{}
	credentialStates.Range(func(k, v any) bool {
		out = append(out, EndpointCredential{Endpoint: k.(string), State: v.(CredentialState)})
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Endpoint < out[j].Endpoint })
	return out
}

type mintFlight struct {
	done chan struct{}
	err  error // written before done closes
}

// credentialCache holds one pool's minted credential. At most one mint runs
// at a time, and the mutex is never held while it runs: connections that
// still have a usable token proceed while a refresh is in flight.
type credentialCache struct {
	endpoint string // "" leaves the cache untracked by CredentialStatus
	mint     mintCredential
	mu       sync.Mutex
	cached   *postgres.DatabaseCredential
	flight   *mintFlight
	state    CredentialState
	lastWarn time.Time
}

type poolOption func(*credentialCache)

// trackedAs publishes the cache's refresh state under an endpoint name.
func trackedAs(endpoint string) poolOption {
	return func(c *credentialCache) { c.endpoint = endpoint }
}

func (c *credentialCache) remainingLocked() time.Duration {
	if c.cached == nil || c.cached.ExpireTime == nil {
		return 0
	}
	return time.Until(c.cached.ExpireTime.AsTime())
}

// token returns a credential for one new physical connection.
func (c *credentialCache) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	left := c.remainingLocked()
	if left > refreshAhead || (c.flight != nil && left > minUsable) {
		defer c.mu.Unlock()
		return c.cached.Token, nil
	}
	if f := c.flight; f != nil {
		c.mu.Unlock()
		return c.awaitFlight(ctx, f)
	}
	f := &mintFlight{done: make(chan struct{})}
	c.flight = f
	c.mu.Unlock()
	c.refresh(ctx, f)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offerLocked(f.err)
}

// awaitFlight waits for another connection's mint instead of starting one
// and shares its outcome: a successful mint is used as the leader uses it,
// even when it is short-lived.
func (c *credentialCache) awaitFlight(ctx context.Context, f *mintFlight) (string, error) {
	select {
	case <-f.done:
	case <-ctx.Done():
		return "", fmt.Errorf("refresh Lakebase database credential: %w", ctx.Err())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if f.err == nil || c.remainingLocked() > minUsable {
		return c.cached.Token, nil
	}
	return "", fmt.Errorf("refresh Lakebase database credential: %w", f.err)
}

// refresh runs one bounded mint and records it. The deferred bookkeeping
// releases waiters even if the mint panics.
func (c *credentialCache) refresh(ctx context.Context, f *mintFlight) {
	var next *postgres.DatabaseCredential
	err := errors.New("lakebase database credential mint did not complete")
	defer func() {
		c.mu.Lock()
		c.flight = nil
		c.recordLocked(next, err)
		f.err = err
		c.mu.Unlock()
		close(f.done)
	}()
	mintCtx, cancel := context.WithTimeout(ctx, mintTimeout)
	defer cancel()
	next, err = c.mint(mintCtx)
	if err == nil && (next == nil || next.Token == "" || next.ExpireTime == nil ||
		time.Until(next.ExpireTime.AsTime()) <= 0) {
		err = errBadCredential
	}
}

func (c *credentialCache) recordLocked(next *postgres.DatabaseCredential, err error) {
	if err == nil {
		c.cached = next
		c.state = CredentialState{LastMintAt: time.Now(), ExpiresAt: next.ExpireTime.AsTime()}
	} else {
		c.state.ConsecutiveFailures++
		c.state.LastError = err.Error()
		c.state.LastErrorClass = mintErrorClass(err)
	}
	if c.endpoint != "" {
		credentialStates.Store(c.endpoint, c.state)
	}
}

// offerLocked serves the result of this connection's own mint: the fresh
// token, the still-usable cached token after a failure, or the error.
func (c *credentialCache) offerLocked(mintErr error) (string, error) {
	if mintErr == nil {
		return c.cached.Token, nil
	}
	left := c.remainingLocked()
	if left <= minUsable {
		return "", fmt.Errorf("refresh Lakebase database credential: %w", mintErr)
	}
	if now := time.Now(); now.Sub(c.lastWarn) >= fallbackWarnEvery {
		c.lastWarn = now
		credentialWarn("lakebase credential refresh failed; using the cached credential",
			"endpoint", c.endpoint, "consecutive_failures", c.state.ConsecutiveFailures,
			"cached_valid_for", left.Round(time.Second).String(), "error", mintErr.Error())
	}
	return c.cached.Token, nil
}

// mintErrorClass names a mint failure for status pages. It never carries the
// error text, which can hold request details.
func mintErrorClass(err error) string {
	switch {
	case errors.Is(err, errBadCredential):
		return "bad_credential"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, apierr.ErrDeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled), errors.Is(err, apierr.ErrCancelled):
		return "canceled"
	case errors.Is(err, apierr.ErrUnauthenticated):
		return "unauthenticated"
	case errors.Is(err, apierr.ErrPermissionDenied):
		return "permission_denied"
	case errors.Is(err, apierr.ErrNotFound):
		return "not_found"
	case errors.Is(err, apierr.ErrTooManyRequests):
		return "throttled"
	case errors.Is(err, apierr.ErrTemporarilyUnavailable), errors.Is(err, apierr.ErrInternalError):
		return "unavailable"
	default:
		return "mint_error"
	}
}
