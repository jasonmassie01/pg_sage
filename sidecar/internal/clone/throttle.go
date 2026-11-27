package clone

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/databricks/databricks-sdk-go/apierr"
)

// Create throttling (lab-05a): platform owners throttle branch and
// restore creation, so every deployment makes at most
// clone.max_creates_per_hour clones (all purposes, counted from the
// receipts), and a provider that answers 429 or 5xx is not asked again
// until an exponential backoff ends.

// DefaultMaxCreatesPerHour is clone.max_creates_per_hour's default.
const DefaultMaxCreatesPerHour = 4

// Throttling errors.
var (
	ErrCreateRateLimited = errors.New("clone: hourly create limit reached")
	ErrProviderBackoff   = errors.New("clone: provider is backing off after throttling")
)

// ProviderStatusError is a provider API reply with an HTTP status.
type ProviderStatusError struct {
	Op         string
	StatusCode int
}

func (e *ProviderStatusError) Error() string {
	return fmt.Sprintf("%s failed with status %d", e.Op, e.StatusCode)
}

// Throttled reports whether err is a provider's 429 or 5xx reply.
func Throttled(err error) bool {
	if err == nil {
		return false
	}
	code := 0
	var status *ProviderStatusError
	var api *apierr.APIError
	switch {
	case errors.As(err, &status):
		code = status.StatusCode
	case errors.As(err, &api):
		code = api.StatusCode
	}
	return code == http.StatusTooManyRequests || code >= 500 && code <= 599
}

// Backoff bounds.
const (
	minCreateBackoff = time.Minute
	maxCreateBackoff = time.Hour
)

// CreateBackoff is one provider's create backoff, shared by all purposes.
type CreateBackoff struct {
	mu       sync.Mutex
	now      func() time.Time
	failures int
	until    time.Time
}

// NewCreateBackoff is a backoff on now's clock (nil: time.Now).
func NewCreateBackoff(now func() time.Time) *CreateBackoff {
	if now == nil {
		now = time.Now
	}
	return &CreateBackoff{now: now}
}

// Allow is nil when a create may be tried, else ErrProviderBackoff with
// when the backoff ends.
func (b *CreateBackoff) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now := b.now(); now.Before(b.until) {
		return fmt.Errorf("%w: %d throttled creates in a row; next try after %s (in %s)",
			ErrProviderBackoff, b.failures, b.until.UTC().Format(time.RFC3339),
			b.until.Sub(now).Round(time.Second))
	}
	return nil
}

// Observe records a create's outcome: a throttled reply doubles the
// backoff (1m, 2m, 4m ... 1h), a success resets it, any other error
// leaves it as it is.
func (b *CreateBackoff) Observe(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case err == nil:
		b.failures, b.until = 0, time.Time{}
	case Throttled(err):
		wait := minCreateBackoff << min(b.failures, 6)
		b.failures++
		b.until = b.now().Add(min(wait, maxCreateBackoff))
	}
}

var sharedBackoffs sync.Map // adapter + "\x00" + scope -> *CreateBackoff

// SharedCreateBackoff is the process's backoff for one adapter and scope.
func SharedCreateBackoff(adapter, scope string) *CreateBackoff {
	b, _ := sharedBackoffs.LoadOrStore(adapter+"\x00"+scope, NewCreateBackoff(nil))
	return b.(*CreateBackoff)
}
