package llm

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// errClass is how a provider failure is treated: whether it is retried,
// whether it counts toward the circuit breaker, and what the status
// reports as last_error_class.
type errClass string

const (
	classNone errClass = ""
	// classCaller is a request the provider rejected (400, 404, 413, 422,
	// other 4xx): the prompt is wrong, not the provider. Never counted.
	classCaller errClass = "caller"
	// classAuth is 401/403: the key or workspace token is wrong. It opens
	// the breaker at once.
	classAuth errClass = "auth"
	// classRate is 429: retried after Retry-After, counted when the last
	// attempt is still limited.
	classRate errClass = "rate"
	// classServer is a 5xx (or an unexpected status): 500, 502, 503 and
	// 504 are retried, then the request counts once.
	classServer errClass = "server"
	// classTimeout is an attempt that ran out of time (or a 408).
	classTimeout errClass = "timeout"
	// classTransport is a connection that failed before a reply.
	classTransport errClass = "transport"
	// classMalformed is a 200 whose body is not a usable completion.
	classMalformed errClass = "malformed"
	// classCanceled is a request the caller or a Reconfigure stopped. It
	// says nothing about the provider and is never counted.
	classCanceled errClass = "canceled"
)

// ErrRequestRejected identifies a request the provider refused as
// invalid (for example a prompt over the context length). It is the
// caller's to fix and never opens the circuit breaker.
var ErrRequestRejected = errors.New("LLM provider rejected the request")

// ErrCircuitOpen identifies a request refused locally because the
// circuit breaker is open (or its single half-open probe is in flight).
var ErrCircuitOpen = errors.New("LLM circuit breaker open")

// classify maps one provider exchange to its class: err is a transport
// error (no reply), otherwise status is the reply's HTTP status. The body
// is accepted so a class can depend on it; none does today.
func classify(status int, _ []byte, err error) errClass {
	if err != nil {
		return transportClass(err)
	}
	switch {
	case status == 0, status == http.StatusOK:
		return classNone
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return classAuth
	case status == http.StatusTooManyRequests:
		return classRate
	case status == http.StatusRequestTimeout:
		return classTimeout
	case status >= 400 && status < 500:
		return classCaller
	default:
		return classServer
	}
}

func transportClass(err error) errClass {
	if errors.Is(err, context.DeadlineExceeded) {
		return classTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return classTimeout
	}
	return classTransport
}

// retryableStatus reports the statuses worth another attempt.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

// providerFailure is a classified provider error. Unwrap keeps the
// underlying error (context.DeadlineExceeded, ErrRateLimited, ...).
type providerFailure struct {
	class  errClass
	status int
	err    error
}

func (e *providerFailure) Error() string { return e.err.Error() }
func (e *providerFailure) Unwrap() error { return e.err }

func (e *providerFailure) Is(target error) bool {
	switch target {
	case ErrRequestRejected:
		return e.class == classCaller
	case ErrRateLimited:
		return e.class == classRate
	}
	return false
}

// ErrorClass names the class of a provider error returned by Chat,
// ChatResult or ChatWithTools: caller, auth, rate, server, timeout,
// transport, malformed or canceled. Any other error is "".
func ErrorClass(err error) string {
	var pf *providerFailure
	if errors.As(err, &pf) {
		return string(pf.class)
	}
	return ""
}

// maxRetryAfter caps how long a provider's Retry-After can hold a retry.
const maxRetryAfter = 30 * time.Second

// parseRetryAfter reads a Retry-After header as delay seconds or an
// HTTP-date (RFC 9110), capped at maxRetryAfter. A date in the past is a
// zero wait; anything unparseable reports false.
func parseRetryAfter(header string, now time.Time) (time.Duration, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(header, 10, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		if secs >= int64(maxRetryAfter/time.Second) {
			return maxRetryAfter, true
		}
		return time.Duration(secs) * time.Second, true
	}
	when, err := http.ParseTime(header)
	if err != nil {
		return 0, false
	}
	wait := when.Sub(now)
	return min(max(wait, 0), maxRetryAfter), true
}
