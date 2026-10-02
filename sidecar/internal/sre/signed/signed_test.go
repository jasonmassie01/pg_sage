package signed

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Signed machine ingestion (AI-SRE-SPEC §9, Codex §6): change events and
// pushed SLI counters are accepted only with an HMAC-SHA256 signature
// over the timestamp, method, path and body, and only inside the
// timestamp tolerance. Every failure is a distinguishable error.

var (
	secret = []byte("0123456789abcdef0123456789abcdef")
	now    = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	body   = []byte(`{"source":"ci","event_id":"run-1"}`)
)

func signedRequest(t *testing.T, at time.Time) Request {
	t.Helper()
	ts, sig := Sign(secret, "POST", "/api/v1/sre/change-events", at, body)
	return Request{Method: "POST", Path: "/api/v1/sre/change-events", Timestamp: ts,
		Signature: sig, Body: body}
}

func TestVerify_AcceptsAFreshSignature(t *testing.T) {
	req := signedRequest(t, now.Add(-30*time.Second))
	at, err := Verify(secret, req, now, 5*time.Minute)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !at.Equal(now.Add(-30 * time.Second)) {
		t.Fatalf("signed at %s, want %s", at, now.Add(-30*time.Second))
	}
	if !strings.HasPrefix(req.Signature, "v1=") || len(req.Signature) != 3+64 {
		t.Fatalf("signature %q is not v1=<64 hex>", req.Signature)
	}
	if req.Timestamp != strconv.FormatInt(now.Add(-30*time.Second).Unix(), 10) {
		t.Fatalf("timestamp header %q", req.Timestamp)
	}
}

// The signature binds every part of the request: a changed body, method,
// path or timestamp, or another secret, does not verify.
func TestVerify_RejectsAnyTamperedPart(t *testing.T) {
	cases := map[string]func(r *Request){
		"body":      func(r *Request) { r.Body = []byte(`{"source":"ci","event_id":"run-2"}`) },
		"method":    func(r *Request) { r.Method = "PUT" },
		"path":      func(r *Request) { r.Path = "/api/v1/sre/sli/checkout" },
		"timestamp": func(r *Request) { r.Timestamp = strconv.FormatInt(now.Unix(), 10) },
		"signature": func(r *Request) { r.Signature = "v1=" + strings.Repeat("0", 64) },
	}
	for name, tamper := range cases {
		req := signedRequest(t, now.Add(-10*time.Second))
		tamper(&req)
		if _, err := Verify(secret, req, now, time.Minute); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s tampered: err = %v, want ErrInvalid", name, err)
		}
	}
	req := signedRequest(t, now)
	other := []byte("fedcba9876543210fedcba9876543210")
	if _, err := Verify(other, req, now, time.Minute); !errors.Is(err, ErrInvalid) {
		t.Fatalf("other secret: err = %v, want ErrInvalid", err)
	}
}

// Boundaries: exactly at the tolerance is accepted, one second beyond it
// (in the past or the future) is stale.
func TestVerify_TimestampToleranceBoundaries(t *testing.T) {
	tol := 5 * time.Minute
	for _, c := range []struct {
		offset time.Duration
		want   error
	}{
		{-tol, nil}, {tol, nil}, {-tol - time.Second, ErrStale}, {tol + time.Second, ErrStale},
		{-24 * time.Hour, ErrStale},
	} {
		_, err := Verify(secret, signedRequest(t, now.Add(c.offset)), now, tol)
		if !errors.Is(err, c.want) && !(c.want == nil && err == nil) {
			t.Errorf("offset %s: err = %v, want %v", c.offset, err, c.want)
		}
	}
}

// A zero or negative tolerance falls back to the default (5 minutes),
// never to "accept anything" or "accept nothing".
func TestVerify_NonPositiveToleranceUsesDefault(t *testing.T) {
	if DefaultTolerance != 5*time.Minute {
		t.Fatalf("DefaultTolerance = %s", DefaultTolerance)
	}
	for _, tol := range []time.Duration{0, -time.Second} {
		if _, err := Verify(secret, signedRequest(t, now.Add(-4*time.Minute)), now,
			tol); err != nil {
			t.Errorf("tolerance %s, 4m old: %v", tol, err)
		}
		_, err := Verify(secret, signedRequest(t, now.Add(-6*time.Minute)), now, tol)
		if !errors.Is(err, ErrStale) {
			t.Errorf("tolerance %s, 6m old: err = %v, want ErrStale", tol, err)
		}
	}
}

func TestVerify_MissingAndMalformedHeaders(t *testing.T) {
	good := signedRequest(t, now)
	cases := map[string]struct {
		mutate func(r *Request)
		want   error
	}{
		"no timestamp":       {func(r *Request) { r.Timestamp = "" }, ErrMissing},
		"no signature":       {func(r *Request) { r.Signature = "" }, ErrMissing},
		"text timestamp":     {func(r *Request) { r.Timestamp = "yesterday" }, ErrMalformed},
		"float timestamp":    {func(r *Request) { r.Timestamp = "1759320000.5" }, ErrMalformed},
		"no version":         {func(r *Request) { r.Signature = r.Signature[3:] }, ErrMalformed},
		"wrong version": {func(r *Request) { r.Signature = "v2=" + r.Signature[3:] },
			ErrMalformed},
		"short hex":          {func(r *Request) { r.Signature = "v1=abcd" }, ErrMalformed},
		"not hex": {func(r *Request) { r.Signature = "v1=" + strings.Repeat("z", 64) },
			ErrMalformed},
		"upper-case version": {func(r *Request) { r.Signature = "V1=" + r.Signature[3:] },
			ErrMalformed},
	}
	for name, c := range cases {
		req := good
		c.mutate(&req)
		if _, err := Verify(secret, req, now, time.Minute); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
}

// Without a configured secret nothing verifies, not even a request
// signed with the empty key.
func TestVerify_EmptySecretIsNotConfigured(t *testing.T) {
	ts, sig := Sign(nil, "POST", "/p", now, body)
	req := Request{Method: "POST", Path: "/p", Timestamp: ts, Signature: sig, Body: body}
	for _, s := range [][]byte{nil, {}} {
		if _, err := Verify(s, req, now, time.Minute); !errors.Is(err, ErrNotConfigured) {
			t.Errorf("secret %v: err = %v, want ErrNotConfigured", s, err)
		}
	}
}

// An empty body is signed and verified like any other body.
func TestVerify_EmptyBody(t *testing.T) {
	ts, sig := Sign(secret, "POST", "/p", now, nil)
	req := Request{Method: "POST", Path: "/p", Timestamp: ts, Signature: sig}
	if _, err := Verify(secret, req, now, time.Minute); err != nil {
		t.Fatalf("empty body: %v", err)
	}
	req.Body = []byte("x")
	if _, err := Verify(secret, req, now, time.Minute); !errors.Is(err, ErrInvalid) {
		t.Fatalf("body added after signing: err = %v", err)
	}
}

// Verify keeps no state, so concurrent verification is safe.
func TestVerify_Concurrent(t *testing.T) {
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := signedRequest(t, now.Add(-time.Duration(i)*time.Second))
			if i%2 == 1 {
				req.Body = []byte("tampered")
			}
			_, err := Verify(secret, req, now, 5*time.Minute)
			if (i%2 == 0) != (err == nil) {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("unexpected result: %v", err)
	}
}
