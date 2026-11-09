package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/evidence"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/siem"
)

// AuditDeps serves the E2 audit routes (§6.17): chain verification,
// evidence packs and SIEM delivery health. All are admin-only.
type AuditDeps struct {
	// Pool resolves a database name ("control" for the control database)
	// to the pool whose audit chains it reads; nil = unknown.
	Pool func(name string) *pgxpool.Pool
	// Control records the evidence exports in sage.auth_audit.
	Control *pgxpool.Pool
	// SIEMStatus reports the sinks' health; nil when export is off.
	SIEMStatus func() []siem.SinkStatus
	// SIEMReanchor moves a sink's cursor back after a chain regressed
	// (siem.Exporter.Reanchor); nil when export is off.
	SIEMReanchor func(context.Context, siem.ReanchorRequest) (siem.ReanchorResult, error)
	// EvidenceKey signs evidence manifests; nil leaves packs hashed only.
	EvidenceKey ed25519.PrivateKey
}

// auditEvidenceExported is the auth_audit event of an evidence download.
const auditEvidenceExported = "evidence_exported"

func registerAuditRoutes(mux *http.ServeMux, deps AuditDeps) {
	adminOnly := RequireRole("admin")
	mux.Handle("GET /api/v1/audit/verify", adminOnly(auditVerifyHandler(deps)))
	mux.Handle("GET /api/v1/audit/evidence", adminOnly(auditEvidenceHandler(deps)))
	mux.Handle("GET /api/v1/audit/siem", adminOnly(auditSIEMHandler(deps)))
	mux.Handle("POST /api/v1/audit/siem/{sink}/reanchor",
		adminOnly(auditSIEMReanchorHandler(deps)))
}

// fleetAuditPool resolves audit sources: "control" is the control pool,
// any other name a fleet database.
func fleetAuditPool(mgr *fleet.DatabaseManager, control *pgxpool.Pool) func(string) *pgxpool.Pool {
	return func(name string) *pgxpool.Pool {
		if name == "control" {
			return control
		}
		if mgr == nil || name == "" || name == "all" {
			return nil
		}
		return mgr.PoolForDatabase(name)
	}
}

func (d AuditDeps) pool(w http.ResponseWriter, r *http.Request) (*pgxpool.Pool, string, bool) {
	name := r.URL.Query().Get("database")
	if validateDatabaseParam(name) != nil || d.Pool == nil {
		jsonError(w, "unknown database", http.StatusNotFound)
		return nil, "", false
	}
	p := d.Pool(name)
	if p == nil {
		jsonError(w, "unknown database", http.StatusNotFound)
		return nil, "", false
	}
	return p, name, true
}

// policyPool is where the standing policy lives: the control database
// (fleet and standalone alike), or the audited pool when none is wired.
func (d AuditDeps) policyPool(audited *pgxpool.Pool) *pgxpool.Pool {
	if d.Control != nil {
		return d.Control
	}
	return audited
}

// auditVerifyTimeout bounds a verification that may read a whole chain
// (full=true, or the first one before any checkpoint exists).
const auditVerifyTimeout = 5 * time.Minute

// auditVerifyHandler verifies chains: since each one's newest checkpoint
// by default (in full when it has none), the whole chain with full=true,
// or an explicit from_seq/to_seq window.
func auditVerifyHandler(d AuditDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pool, name, ok := d.pool(w, r)
		if !ok {
			return
		}
		req, err := parseVerifyRequest(r)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		specs, err := chainsToVerify(r, pool)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		ctx, cancel := req.context(w, r)
		defer cancel()
		reports := []auditchain.Report{}
		ok = true
		for _, spec := range specs {
			rep, err := req.verify(ctx, pool, spec)
			if errors.Is(err, context.DeadlineExceeded) {
				jsonError(w, fmt.Sprintf("verifying %s took longer than %s; verify it "+
					"in from_seq/to_seq windows", spec.Chain, auditVerifyTimeout),
					http.StatusGatewayTimeout)
				return
			}
			if err != nil {
				internalError(w, r, "verify audit chain", err)
				return
			}
			ok = ok && rep.OK()
			reports = append(reports, rep)
		}
		jsonResponse(w, map[string]any{"database": name, "ok": ok, "reports": reports})
	}
}

// verifyRequest is how GET /api/v1/audit/verify was asked to verify.
type verifyRequest struct {
	window auditchain.Window
	full   bool
}

func parseVerifyRequest(r *http.Request) (verifyRequest, error) {
	window, err := seqWindow(r)
	if err != nil {
		return verifyRequest{}, err
	}
	req := verifyRequest{window: window}
	if raw := r.URL.Query().Get("full"); raw != "" {
		if req.full, err = strconv.ParseBool(raw); err != nil {
			return verifyRequest{}, fmt.Errorf("full must be true or false")
		}
	}
	if req.full && req.windowed() {
		return verifyRequest{}, fmt.Errorf("full=true verifies the whole chain; " +
			"leave out from_seq and to_seq")
	}
	return req, nil
}

func (q verifyRequest) windowed() bool { return q.window.FromSeq > 0 || q.window.ToSeq > 0 }

// context bounds a verification that may read a whole chain, and lets its
// response outlive the server's write timeout. A window keeps the request's.
func (q verifyRequest) context(w http.ResponseWriter, r *http.Request) (context.Context,
	context.CancelFunc) {
	if q.windowed() {
		return r.Context(), func() {}
	}
	deadline := time.Now().Add(auditVerifyTimeout + 5*time.Second)
	if err := http.NewResponseController(w).SetWriteDeadline(deadline); err != nil &&
		!errors.Is(err, http.ErrNotSupported) {
		slog.Warn("audit verify: the response may hit the server's write timeout",
			"error", err)
	}
	return context.WithTimeout(r.Context(), auditVerifyTimeout)
}

func (q verifyRequest) verify(ctx context.Context, pool *pgxpool.Pool,
	spec auditchain.Spec) (auditchain.Report, error) {
	if q.windowed() {
		return auditchain.Verify(ctx, pool, spec, q.window)
	}
	return auditchain.VerifyFromCheckpoint(ctx, pool, spec, q.full)
}

func seqWindow(r *http.Request) (auditchain.Window, error) {
	var w auditchain.Window
	for key, dst := range map[string]*int64{"from_seq": &w.FromSeq, "to_seq": &w.ToSeq} {
		raw := r.URL.Query().Get(key)
		if raw == "" {
			continue
		}
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			return w, fmt.Errorf("%s must be a non-negative integer", key)
		}
		*dst = v
	}
	if w.ToSeq > 0 && w.FromSeq > w.ToSeq {
		return w, fmt.Errorf("from_seq must not exceed to_seq")
	}
	return w, nil
}

// chainsToVerify is the requested chain, or every chain installed.
func chainsToVerify(r *http.Request, pool *pgxpool.Pool) ([]auditchain.Spec, error) {
	if chain := r.URL.Query().Get("chain"); chain != "" {
		spec, ok := auditchain.SpecFor(chain)
		if !ok {
			return nil, fmt.Errorf("unknown chain %q", chain)
		}
		return []auditchain.Spec{spec}, nil
	}
	names, err := auditchain.Installed(r.Context(), pool)
	if err != nil {
		return nil, err
	}
	var out []auditchain.Spec
	for _, n := range names {
		if spec, ok := auditchain.SpecFor(n); ok {
			out = append(out, spec)
		}
	}
	return out, nil
}

func auditSIEMHandler(d AuditDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		sinks := []siem.SinkStatus{}
		if d.SIEMStatus != nil {
			sinks = append(sinks, d.SIEMStatus()...)
		}
		jsonResponse(w, map[string]any{"enabled": d.SIEMStatus != nil, "sinks": sinks})
	}
}

func auditEvidenceHandler(d AuditDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pool, name, ok := d.pool(w, r)
		if !ok {
			return
		}
		scope, err := evidenceScope(r, name)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		pack, err := evidence.BuildWithPolicy(r.Context(), pool, d.policyPool(pool), scope,
			d.EvidenceKey, time.Now())
		switch {
		case errors.Is(err, evidence.ErrScope):
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		case errors.Is(err, evidence.ErrNotFound):
			jsonError(w, "no such action", http.StatusNotFound)
			return
		case err != nil:
			internalError(w, r, "build evidence pack", err)
			return
		}
		var buf bytes.Buffer
		if err := pack.WriteTarGz(&buf); err != nil {
			internalError(w, r, "write evidence pack", err)
			return
		}
		recordEvidenceExport(r, d.Control, scope)
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(
			`attachment; filename="%s"`, evidenceFilename(scope)))
		w.Header().Set("X-Evidence-Signed", strconv.FormatBool(d.EvidenceKey != nil))
		_, _ = w.Write(buf.Bytes())
	}
}

func evidenceScope(r *http.Request, database string) (evidence.Scope, error) {
	q := r.URL.Query()
	s := evidence.Scope{Database: database}
	if raw := q.Get("action_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return s, fmt.Errorf("action_id must be a positive integer")
		}
		s.ActionID = id
	}
	for key, dst := range map[string]*time.Time{"from": &s.From, "to": &s.To} {
		if raw := q.Get(key); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return s, fmt.Errorf("%s must be an RFC 3339 time", key)
			}
			*dst = t
		}
	}
	return s, nil
}

func evidenceFilename(s evidence.Scope) string {
	if s.ActionID > 0 {
		return fmt.Sprintf("pg_sage-evidence-%s-action-%d.tar.gz", s.Database, s.ActionID)
	}
	return fmt.Sprintf("pg_sage-evidence-%s-%s.tar.gz", s.Database,
		s.From.UTC().Format("20060102T150405Z"))
}

// recordEvidenceExport audits who exported which evidence.
func recordEvidenceExport(r *http.Request, control *pgxpool.Pool, s evidence.Scope) {
	actor := 0
	if u := UserFromContext(r.Context()); u != nil {
		actor = u.ID
	}
	detail := map[string]any{"database": s.Database}
	if s.ActionID > 0 {
		detail["action_id"] = s.ActionID
	} else {
		detail["from"], detail["to"] = s.From.UTC(), s.To.UTC()
	}
	recordAuthEvent(r, control, auth.AuthAuditEvent{Event: auditEvidenceExported,
		ActorUserID: actor, Detail: detail})
}
