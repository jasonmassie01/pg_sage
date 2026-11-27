package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/siem"
)

// auditSIEMReanchored is the auth_audit event of a SIEM cursor re-anchor.
const auditSIEMReanchored = "siem_reanchored"

const maxReanchorBody = 4 << 10

// auditSIEMReanchorHandler serves POST /api/v1/audit/siem/{sink}/reanchor
// (admin): after a chain went back behind a sink's cursor, export resumes
// at from_seq (default 1) once an admin compared the SIEM's copy with the
// database. Re-anchoring is never automatic.
func auditSIEMReanchorHandler(d AuditDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.SIEMReanchor == nil {
			jsonError(w, "SIEM export is off", http.StatusNotFound)
			return
		}
		req, err := reanchorRequest(w, r)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		res, err := d.SIEMReanchor(r.Context(), req)
		if err != nil {
			writeReanchorError(w, r, err)
			return
		}
		actor := 0
		if u := UserFromContext(r.Context()); u != nil {
			actor = u.ID
		}
		recordAuthEvent(r, d.Control, auth.AuthAuditEvent{Event: auditSIEMReanchored,
			ActorUserID: actor, Detail: map[string]any{"sink": res.Sink,
				"source": res.Source, "chain": res.Chain, "from_seq": res.FromSeq,
				"previous_seq": res.PreviousSeq, "fork_to": res.ForkTo}})
		jsonResponse(w, res)
	}
}

// reanchorRequest reads and validates the body; from_seq 0 or absent is 1.
func reanchorRequest(w http.ResponseWriter, r *http.Request) (siem.ReanchorRequest, error) {
	var body struct {
		Source  string `json:"source"`
		Chain   string `json:"chain"`
		FromSeq int64  `json:"from_seq"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxReanchorBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return siem.ReanchorRequest{}, fmt.Errorf("malformed body: %w", err)
	}
	if body.Source == "" || validateDatabaseParam(body.Source) != nil {
		return siem.ReanchorRequest{}, errors.New(
			"source must name a database (or \"control\")")
	}
	if _, ok := auditchain.SpecFor(body.Chain); !ok {
		return siem.ReanchorRequest{}, fmt.Errorf("unknown chain %q", body.Chain)
	}
	if body.FromSeq < 0 {
		return siem.ReanchorRequest{}, errors.New("from_seq must be a positive position")
	}
	if body.FromSeq == 0 {
		body.FromSeq = 1
	}
	return siem.ReanchorRequest{Sink: r.PathValue("sink"), Source: body.Source,
		Chain: body.Chain, FromSeq: body.FromSeq}, nil
}

func writeReanchorError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, siem.ErrUnknownSink), errors.Is(err, siem.ErrUnknownSource),
		errors.Is(err, siem.ErrNoCursor):
		jsonError(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, siem.ErrNotLeader), errors.Is(err, siem.ErrFenced):
		jsonError(w, err.Error(), http.StatusConflict)
	case errors.Is(err, siem.ErrInvalidReanchor):
		jsonError(w, err.Error(), http.StatusBadRequest)
	default:
		internalError(w, r, "re-anchor SIEM cursor", err)
	}
}
