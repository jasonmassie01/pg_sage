package specialist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// stream serves server-sent events: one "status" event per change, then
// "end" when the investigation is terminal or the stream window closes
// (the client reconnects). The first status call authorizes the stream
// (scope, database, rate limit); later polls are part of the same call.
func (h *handler) stream(w http.ResponseWriter, r *http.Request, id Identity) {
	database, invID := r.PathValue("db"), r.PathValue("id")
	first, err := h.svc.Status(r.Context(), id, database, invID)
	if err != nil {
		writeError(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, fmt.Errorf("%w: streaming unsupported", ErrUnavailable))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	h.pump(r.Context(), w, flusher, database, sre.UUID(first.Investigation.ID), first)
}

func (h *handler) pump(ctx context.Context, w http.ResponseWriter, f http.Flusher,
	database string, uid sre.UUID, st StatusResponse) {
	window := time.NewTimer(h.opts.StreamWindow)
	defer window.Stop()
	tick := time.NewTicker(h.opts.StreamInterval)
	defer tick.Stop()
	last := sendEvent(w, f, "status", st, nil)
	for {
		if st.Investigation.Terminal {
			sendEvent(w, f, "end", endEvent("terminal"), nil)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-window.C:
			sendEvent(w, f, "end", endEvent("window"), nil)
			return
		case <-tick.C:
		}
		next, err := h.svc.poll(ctx, database, uid)
		if err != nil {
			c := codeOf(err)
			sendEvent(w, f, "error", ErrorResponse{ContractVersion: ContractVersion,
				Error: c.msg, Code: c.code}, nil)
			return
		}
		st = next
		last = sendEvent(w, f, "status", st, last)
	}
}

type streamEnd struct {
	ContractVersion string `json:"contract_version"`
	Reason          string `json:"reason"`
}

func endEvent(reason string) streamEnd {
	return streamEnd{ContractVersion: ContractVersion, Reason: reason}
}

// sendEvent writes one event unless its data equals previous; it returns
// the data written (or previous).
func sendEvent(w http.ResponseWriter, f http.Flusher, event string, v any,
	previous []byte) []byte {
	data, err := json.Marshal(v)
	if err != nil || bytes.Equal(data, previous) {
		return previous
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	f.Flush()
	return data
}

// poll reads a status for an already authorized stream.
func (s *Service) poll(ctx context.Context, database string, uid sre.UUID) (StatusResponse,
	error) {
	b, ok := s.dir.Backend(database)
	if !ok {
		return StatusResponse{}, ErrNotFound
	}
	d, err := b.Detail(ctx, uid)
	if err != nil {
		return StatusResponse{}, backendErr(err)
	}
	return s.statusOf(database, d), nil
}
