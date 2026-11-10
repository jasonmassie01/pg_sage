package cloudtel

import (
	"fmt"
	"testing"
	"time"
)

// stsFailure is the AWS chain's error when an assumed or web-identity
// role is refused: the SDK puts a fresh RequestID in every message.
func stsFailure(requestID, code string) error {
	return fmt.Errorf("%w: AWS default credential chain: operation error STS: "+
		"AssumeRoleWithWebIdentity, https response error StatusCode: 403, RequestID: %s, %s: "+
		"not authorized", ErrNoCredentials, requestID, code)
}

// State transition: the same failure with a new request ID on every
// retry is reported once; a changed reason is still reported.
func TestRuntimeRunIgnoresRequestIDs(t *testing.T) {
	src := &stubSource{provider: "rds", errs: []error{
		stsFailure("1b2c3d4e-0001", "InvalidIdentityToken"),
		stsFailure("1b2c3d4e-0002", "InvalidIdentityToken"),
		stsFailure("9f8e7d6c-0003", "InvalidIdentityToken"),
		stsFailure("9f8e7d6c-0004", "AccessDenied"),
	}}
	h := startRun(t, src, time.Minute)
	for i := 0; i < 4; i++ {
		h.fire(h.next())
	}
	h.next()
	if n := h.reports.Load(); n != 2 {
		t.Fatalf("reports = %d, want 2 (first failure, changed reason)", n)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if got := h.reported[1].Error(); got != stsFailure("9f8e7d6c-0004", "AccessDenied").Error() {
		t.Fatalf("second report = %q, want the full error with its request ID", got)
	}
}

// Invalid input: the key keeps everything but the request ID, so two
// errors that differ elsewhere never collapse.
func TestFailureKey(t *testing.T) {
	a := failureKey(stsFailure("aaaa-1", "InvalidIdentityToken"))
	b := failureKey(stsFailure("bbbb-2", "InvalidIdentityToken"))
	c := failureKey(stsFailure("aaaa-1", "AccessDenied"))
	if a != b || a == c {
		t.Fatalf("keys: same failure %q vs %q; changed reason %q", a, b, c)
	}
	plain := fmt.Errorf("%w: no credentials found", ErrNoCredentials)
	if got, want := failureKey(plain), "failed: "+plain.Error(); got != want {
		t.Fatalf("failureKey without a request ID = %q, want %q", got, want)
	}
	if failureKey(ErrThrottled) == failureKey(ErrAuth) {
		t.Fatal("different classes share a key")
	}
}

// State transition: once Run stops, no next poll is advertised.
func TestRuntimeRunClearsNextPollOnStop(t *testing.T) {
	src := &stubSource{provider: "rds", errs: []error{noCreds()}}
	h := startRun(t, src, time.Minute)
	h.next()
	if h.r.Status().NextPollAt == nil {
		t.Fatal("NextPollAt missing while Run waits")
	}
	h.stop()
	if st := h.r.Status(); st.NextPollAt != nil || st.ConsecutiveFailures != 1 {
		t.Fatalf("after stop: next %v failures %d", st.NextPollAt, st.ConsecutiveFailures)
	}
}
