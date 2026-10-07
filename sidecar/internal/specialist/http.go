package specialist

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HandlerOptions configures the HTTP handler.
type HandlerOptions struct {
	PagerDuty *PagerDutyAdapter // nil: the adapter is not configured
	Webhook   *WebhookAdapter   // nil: the adapter is not configured
	// StreamWindow bounds one SSE stream (below the API's request deadline;
	// the client reconnects); StreamInterval is its poll interval.
	StreamWindow   time.Duration
	StreamInterval time.Duration
	Now            func() time.Time
}

// Stream defaults: the API's request deadline is 30 s.
const (
	defaultStreamWindow   = 25 * time.Second
	defaultStreamInterval = time.Second
)

type handler struct {
	svc  *Service
	auth Authenticator
	opts HandlerOptions
}

// NewHandler serves the contract under BasePath. Every route but the
// contract and schema documents needs a bearer MCP token; a session
// cookie never authenticates it.
func NewHandler(svc *Service, auth Authenticator, opts HandlerOptions) http.Handler {
	if opts.StreamWindow <= 0 {
		opts.StreamWindow = defaultStreamWindow
	}
	if opts.StreamInterval <= 0 {
		opts.StreamInterval = defaultStreamInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	h := &handler{svc: svc, auth: auth, opts: opts}
	inv := BasePath + "/databases/{db}/investigations"
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+BasePath+"/contract", h.contract)
	mux.HandleFunc("GET "+BasePath+"/openapi.json", h.openapi)
	mux.Handle("POST "+inv, h.authed("http", h.open))
	mux.Handle("GET "+inv+"/{id}", h.authed("http", h.status))
	mux.Handle("GET "+inv+"/{id}/stream", h.authed("http", h.stream))
	mux.Handle("GET "+inv+"/{id}/result", h.authed("http", h.result))
	mux.Handle("GET "+inv+"/{id}/transcript", h.authed("http", h.transcript))
	mux.Handle("POST "+inv+"/{id}/remediations/{rid}/request", h.authed("http", h.remediate))
	mux.Handle("POST "+BasePath+"/adapters/pagerduty", h.authed("pagerduty", h.pagerDuty))
	mux.Handle("POST "+BasePath+"/adapters/webhook", h.authed("webhook", h.webhook))
	mux.HandleFunc(BasePath+"/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, ErrNotFound)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Sage-Contract-Version", ContractVersion)
		mux.ServeHTTP(w, r)
	})
}

type identityHandler func(w http.ResponseWriter, r *http.Request, id Identity)

// authed authenticates the bearer token and binds the identity with the
// transport it arrived on.
func (h *handler) authed(transport string, next identityHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, secret, found := strings.Cut(r.Header.Get("Authorization"), " ")
		secret = strings.TrimSpace(secret)
		if !found || !strings.EqualFold(scheme, "Bearer") || secret == "" {
			writeError(w, ErrUnauthenticated)
			return
		}
		id, err := h.auth.Authenticate(r.Context(), secret)
		if err != nil {
			if !errors.Is(err, ErrUnauthenticated) {
				slog.Error("specialist: token validation failed", "path", r.URL.Path,
					"err", err)
				err = ErrUnavailable
			}
			writeError(w, err)
			return
		}
		id.Transport = transport
		next(w, r, id)
	})
}

func (h *handler) contract(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, ContractInfo{ContractVersion: ContractVersion,
		SchemaURL: BasePath + "/openapi.json", SupportedVersions: []string{ContractVersion}})
}

func (h *handler) openapi(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openAPIDocument)
}

func (h *handler) open(w http.ResponseWriter, r *http.Request, id Identity) {
	req, err := DecodeOpenRequest(r.Body)
	if err != nil {
		writeError(w, err)
		return
	}
	h.respondOpen(w, r.Context(), id, r.PathValue("db"), req)
}

func (h *handler) respondOpen(w http.ResponseWriter, ctx context.Context, id Identity,
	database string, req OpenRequest) {
	resp, err := h.svc.Open(ctx, id, database, req)
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusOK
	if resp.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, resp)
}

func (h *handler) status(w http.ResponseWriter, r *http.Request, id Identity) {
	resp, err := h.svc.Status(r.Context(), id, r.PathValue("db"), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *handler) result(w http.ResponseWriter, r *http.Request, id Identity) {
	resp, err := h.svc.Result(r.Context(), id, r.PathValue("db"), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusOK
	if !resp.Investigation.Terminal {
		status = http.StatusAccepted
	}
	writeJSON(w, status, resp)
}

func (h *handler) transcript(w http.ResponseWriter, r *http.Request, id Identity) {
	resp, err := h.svc.Transcript(r.Context(), id, r.PathValue("db"), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *handler) remediate(w http.ResponseWriter, r *http.Request, id Identity) {
	req, err := DecodeRemediationRequest(r.Body)
	if err != nil {
		writeError(w, err)
		return
	}
	resp, err := h.svc.RequestRemediation(r.Context(), id, r.PathValue("db"),
		r.PathValue("id"), r.PathValue("rid"), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("specialist: writing the response failed", "err", err)
	}
}

// writeError answers err with its published code; internal errors are
// logged and never echoed.
func writeError(w http.ResponseWriter, err error) {
	c := codeOf(err)
	msg := err.Error()
	if c == errInternal {
		slog.Error("specialist: request failed", "err", err)
		msg = c.msg
	}
	body := ErrorResponse{ContractVersion: ContractVersion, Error: msg, Code: c.code}
	var rl *RateLimitError
	if errors.As(err, &rl) {
		secs := int(math.Ceil(rl.RetryAfter.Seconds()))
		body.RetryAfterSeconds = max(secs, 1)
		w.Header().Set("Retry-After", strconv.Itoa(body.RetryAfterSeconds))
	}
	if c == ErrUnauthenticated {
		w.Header().Set("WWW-Authenticate", `Bearer realm="pg_sage specialist"`)
	}
	writeJSON(w, c.status, body)
}
