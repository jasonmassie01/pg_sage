package api

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/config"
)

// OAuth identity bindings (agentgov-03), admin only:
//
//	GET    /api/v1/agents/{id}/identities                   → {items, truncated}
//	POST   /api/v1/agents/{id}/identities {issuer, subject}  → 201 the binding
//	DELETE /api/v1/agents/{id}/identities?issuer=&subject=  → the removed binding
//	GET    /api/v1/mcp/oauth/issuers                        → {enabled, issuers}
//
// A binding grants exactly the principal's profile and ceiling, so like a
// token mint it takes one admin and is audited (agent_identity_bound). The
// issuer must be exactly one of mcp.oauth.issuers as the server started
// with them (the validator reads the same list), https or loopback http as
// mcp.oauth itself allows. Unbinding narrows: it takes effect at once and
// accepts any issuer, so a binding outlives the removal of its issuer.

const maxIdentityBody = 16 << 10

type agentIdentityRoutes struct {
	store   *agentguard.Store
	enabled bool
	issuers []string
}

func registerAgentIdentityRoutes(mux *http.ServeMux, pool *pgxpool.Pool,
	cfg *config.Config) {
	h := &agentIdentityRoutes{store: agentguard.NewStore(pool), issuers: []string{}}
	if cfg != nil && cfg.MCP.OAuth.Enabled {
		h.enabled = true
		for _, iss := range cfg.MCP.OAuth.Issuers {
			h.issuers = append(h.issuers, iss.Issuer)
		}
	}
	const base = "/api/v1/agents/{id}/identities"
	route(mux, "GET "+base, gateAdmin, http.HandlerFunc(h.list))
	route(mux, "POST "+base, gateAdmin, http.HandlerFunc(h.bind))
	route(mux, "DELETE "+base, gateAdmin, http.HandlerFunc(h.unbind))
	route(mux, "GET /api/v1/mcp/oauth/issuers", gateAdmin, http.HandlerFunc(h.listIssuers))
}

func (h *agentIdentityRoutes) listIssuers(w http.ResponseWriter, _ *http.Request) {
	jsonResponse(w, map[string]any{"enabled": h.enabled, "issuers": h.issuers})
}

func (h *agentIdentityRoutes) list(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.ListIdentities(r.Context(), r.PathValue("id"))
	if err != nil {
		identityError(w, r, "list agent identities", err)
		return
	}
	jsonResponse(w, out)
}

type bindIdentityBody struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

func (h *agentIdentityRoutes) bind(w http.ResponseWriter, r *http.Request) {
	var body bindIdentityBody
	if !decodeIdentityBody(w, r, &body) {
		return
	}
	if msg := h.issuerProblem(body.Issuer); msg != "" {
		invalidIdentity(w, msg)
		return
	}
	if msg := subjectProblem(body.Subject); msg != "" {
		invalidIdentity(w, msg)
		return
	}
	b, err := h.store.BindIdentity(r.Context(), r.PathValue("id"), agentguard.BindRequest{
		Issuer: body.Issuer, Subject: body.Subject, CreatedBy: authenticatedActor(r),
		Audit: governanceAudit(r, "")})
	if err != nil {
		identityError(w, r, "bind agent identity", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(b)
}

func (h *agentIdentityRoutes) unbind(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	issuer, subject := q.Get("issuer"), q.Get("subject")
	if issuer == "" {
		invalidIdentity(w, "issuer is required")
		return
	}
	if msg := subjectProblem(subject); msg != "" {
		invalidIdentity(w, msg)
		return
	}
	b, err := h.store.UnbindIdentity(r.Context(), r.PathValue("id"), issuer, subject,
		governanceAudit(r, ""))
	if err != nil {
		identityError(w, r, "unbind agent identity", err)
		return
	}
	jsonResponse(w, b)
}

// decodeIdentityBody reads one JSON object, refusing unknown fields,
// wrong types and trailing data with 400.
func decodeIdentityBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxIdentityBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(dst)
	if err == nil && dec.Decode(&struct{}{}) != io.EOF {
		err = errors.New("trailing data after the JSON object")
	}
	if err != nil {
		writeJSONCode(w, http.StatusBadRequest, "invalid_body",
			"invalid request body: want {\"issuer\", \"subject\"}: "+err.Error())
		return false
	}
	return true
}

// issuerProblem says why issuer cannot be bound, or "" when it can.
func (h *agentIdentityRoutes) issuerProblem(issuer string) string {
	if !h.enabled {
		return "OAuth is off: set mcp.oauth.enabled and list the issuer in " +
			"mcp.oauth.issuers, then restart"
	}
	if issuer == "" {
		return "issuer is required"
	}
	if !secureIssuerURL(issuer) {
		return "issuer must be an https URL (http only on a loopback host)"
	}
	for _, iss := range h.issuers {
		if iss == issuer {
			return ""
		}
	}
	return "issuer is not one of mcp.oauth.issuers; it must match one exactly"
}

// secureIssuerURL accepts https, and http only on a loopback host, the
// rule mcp.oauth applies to issuer URLs.
func secureIssuerURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		ip := net.ParseIP(host)
		return host == "localhost" || (ip != nil && ip.IsLoopback())
	}
	return false
}

// subjectProblem says why subject is unusable, or "" when it is fine.
func subjectProblem(subject string) string {
	switch {
	case subject == "":
		return "subject is required"
	case len(subject) > agentguard.MaxSubjectBytes:
		return "subject must be at most 255 bytes"
	case strings.IndexFunc(subject, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0:
		return "subject must not contain control characters"
	}
	return ""
}

func invalidIdentity(w http.ResponseWriter, msg string) {
	writeJSONCode(w, http.StatusUnprocessableEntity, "invalid_identity", msg)
}

// identityError maps the binding errors, then the principal errors.
func identityError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, agentguard.ErrIdentityBound):
		writeJSONCode(w, http.StatusConflict, "identity_bound",
			"this identity is already bound to an agent; unbind it there first")
	case errors.Is(err, agentguard.ErrIdentityNotBound):
		writeJSONCode(w, http.StatusNotFound, "identity_not_bound",
			"this identity is not bound to the agent")
	case errors.Is(err, agentguard.ErrInvalid):
		invalidIdentity(w, strings.TrimPrefix(err.Error(), "agentguard: "))
	default:
		agentError(w, r, op, err)
	}
}
