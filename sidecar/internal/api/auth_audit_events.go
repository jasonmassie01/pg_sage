package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
)

// clientIPResolver returns the client address for audit rows. The sidecar
// installs the rate limiter's resolver, which honours trusted proxies.
var clientIPResolver atomic.Pointer[func(*http.Request) string]

// SetClientIPResolver sets how auth audit rows resolve the client address;
// nil restores the default (the TCP peer).
func SetClientIPResolver(fn func(*http.Request) string) {
	if fn == nil {
		clientIPResolver.Store(nil)
		return
	}
	clientIPResolver.Store(&fn)
}

func requestClientIP(r *http.Request) string {
	if fn := clientIPResolver.Load(); fn != nil {
		return (*fn)(r)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// recordAuthEvent appends a login or user-administration event to
// sage.auth_audit with the request's client address. A failed write is
// logged with the event name (never the detail) and does not change the
// response: the audited action already happened or was already refused.
func recordAuthEvent(r *http.Request, pool *pgxpool.Pool, ev auth.AuthAuditEvent) {
	if pool == nil {
		return
	}
	ev.SourceIP = requestClientIP(r)
	if err := auth.RecordAuthAudit(context.WithoutCancel(r.Context()), pool, ev); err != nil {
		slog.Error("auth audit write failed", "event", ev.Event,
			"target_user_id", ev.TargetUserID, "error", err)
	}
}

// loginFailureReason maps a login error to its audit reason code.
func loginFailureReason(err error) string {
	switch {
	case errors.Is(err, auth.ErrOIDCNonceMismatch):
		return "nonce_mismatch"
	case errors.Is(err, auth.ErrOIDCIDTokenMissing):
		return "id_token_missing"
	case errors.Is(err, auth.ErrOIDCIDTokenInvalid):
		return "id_token_invalid"
	case errors.Is(err, auth.ErrOIDCSubjectMismatch):
		return "subject_mismatch"
	case errors.Is(err, auth.ErrOAuthStateInvalid):
		return "state_invalid"
	case errors.Is(err, auth.ErrOAuthEmailUnverified):
		return "email_unverified"
	case errors.Is(err, auth.ErrOAuthLinkRequired):
		return "link_required"
	case errors.Is(err, auth.ErrOAuthUnmapped):
		return "not_authorized"
	default:
		return "exchange_failed"
	}
}
