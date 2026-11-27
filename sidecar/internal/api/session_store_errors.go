package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// apiauth-02: when the session store cannot answer, the API says so with a
// retryable 503 instead of the 401 that signs the dashboard out. A failover
// or a saturated pool must not end every operator's session mid-incident.

const (
	authStoreUnavailableCode = "auth_store_unavailable"
	// authStoreRetryAfter is the Retry-After the dashboard waits between
	// session-store retries.
	authStoreRetryAfter  = "5"
	sessionStoreLogEvery = 30 * time.Second
)

// sessionStoreLog logs a session-store outage once per window: every
// dashboard poll hits it, and one line per request would bury the cause.
var sessionStoreLog = newRateLimitedLog(sessionStoreLogEvery, nil)

// writeSessionStoreUnavailable answers 503 auth_store_unavailable and logs
// the cause, rate limited. The cause never reaches the client.
func writeSessionStoreUnavailable(w http.ResponseWriter, r *http.Request, cause error) {
	if errors.Is(r.Context().Err(), context.Canceled) {
		// The client left mid-lookup: nothing says the store is down, so
		// do not raise an outage ERROR or use up the log window.
		slog.Debug("session lookup abandoned by the client",
			"path", r.URL.Path, "error", cause)
	} else {
		sessionStoreLog.Error("session store unavailable; answering 503 "+
			"auth_store_unavailable and keeping sessions",
			"path", r.URL.Path, "error", cause)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", authStoreRetryAfter)
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": "session store unavailable", "code": authStoreUnavailableCode,
	})
}

// rateLimitedLog writes at most one ERROR line per window and counts the
// lines it suppressed in between.
type rateLimitedLog struct {
	mu         sync.Mutex
	every      time.Duration
	logger     *slog.Logger // nil means slog.Default() at write time
	now        func() time.Time
	last       time.Time
	suppressed int
}

func newRateLimitedLog(every time.Duration, logger *slog.Logger) *rateLimitedLog {
	return &rateLimitedLog{every: every, logger: logger, now: time.Now}
}

// Error logs msg unless a line was written less than every ago, and reports
// whether it logged. A logged line carries the count suppressed before it.
func (l *rateLimitedLog) Error(msg string, args ...any) bool {
	l.mu.Lock()
	now := l.now()
	if !l.last.IsZero() && now.Sub(l.last) < l.every {
		l.suppressed++
		l.mu.Unlock()
		return false
	}
	suppressed := l.suppressed
	l.last, l.suppressed = now, 0
	l.mu.Unlock()
	logger := l.logger
	if logger == nil {
		logger = slog.Default()
	}
	if suppressed > 0 {
		args = append(args, "suppressed", suppressed)
	}
	logger.Error(msg, args...)
	return true
}
