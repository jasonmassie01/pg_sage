package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// apiauth-02: ValidateSession tells an invalid cookie (ErrSessionInvalid)
// from a session store that cannot answer (ErrSessionStoreUnavailable), so
// the API can answer 401 for the first and 503 for the second.

// No concurrent access tests here: ValidateSession holds no state of its
// own; concurrent validation is covered in session_hash_test.go.

func TestValidateSession_ClosedPoolIsStoreUnavailable(t *testing.T) {
	tok, err := newSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	u, err := ValidateSession(context.Background(), closedPool(t), tok)
	if u != nil || !errors.Is(err, ErrSessionStoreUnavailable) {
		t.Fatalf("closed pool = (%v, %v), want ErrSessionStoreUnavailable", u, err)
	}
	if errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("closed pool error %v also matches ErrSessionInvalid", err)
	}
	// The cause stays inspectable for the operator log.
	if !strings.Contains(err.Error(), "validating session") ||
		!strings.Contains(err.Error(), "closed pool") {
		t.Fatalf("error %q lost the step or the cause", err)
	}
}

func TestValidateSession_NilPoolWellFormedCookieIsStoreUnavailable(t *testing.T) {
	tok, err := newSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	u, err := ValidateSession(context.Background(), nil, tok)
	if u != nil || !errors.Is(err, ErrSessionStoreUnavailable) {
		t.Fatalf("nil pool = (%v, %v), want ErrSessionStoreUnavailable", u, err)
	}
	if errors.Is(err, ErrSessionInvalid) {
		t.Fatal("a missing session store must not look like an invalid cookie")
	}
}

func TestValidateSession_NilPoolMalformedCookieIsInvalid(t *testing.T) {
	// A cookie that cannot be a session is invalid whether or not the store
	// answers; it must never become a retryable 503.
	for _, tok := range []string{"'; DROP TABLE sage.sessions; --", "x", ""} {
		u, err := ValidateSession(context.Background(), nil, tok)
		if u != nil || !errors.Is(err, ErrSessionInvalid) {
			t.Fatalf("ValidateSession(nil, %q) = (%v, %v), want ErrSessionInvalid",
				tok, u, err)
		}
		if errors.Is(err, ErrSessionStoreUnavailable) {
			t.Fatalf("malformed cookie %q reported as store unavailable", tok)
		}
	}
}

func TestValidateSession_UnknownTokenIsInvalidNotUnavailable(t *testing.T) {
	pool := setupOIDCPool(t)
	tok, err := newSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	u, err := ValidateSession(context.Background(), pool, tok)
	if u != nil || !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("unknown token = (%v, %v), want ErrSessionInvalid", u, err)
	}
	if errors.Is(err, ErrSessionStoreUnavailable) {
		t.Fatal("an unknown session must not look like a store outage")
	}
}

func TestValidateSession_CancelledContextIsStoreUnavailable(t *testing.T) {
	pool := setupOIDCPool(t)
	tok, err := newSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = ValidateSession(ctx, pool, tok)
	if !errors.Is(err, ErrSessionStoreUnavailable) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled lookup = %v, want store unavailable wrapping context.Canceled", err)
	}
}
