package mcp

import (
	"context"
	"net/http"
	"time"
)

// keepAliveInterval is how often an idle subscription stream gets an SSE
// comment, so intermediaries do not close it.
const keepAliveInterval = 15 * time.Second

// serveSubscription answers subscriptions/listen over Streamable HTTP with
// an SSE stream: the acknowledgment, then tools/list_changed whenever the
// tool list changes. Before the request's deadline (the API bounds every
// request) the server ends the stream gracefully with the completion
// result, so the client knows to listen again instead of seeing a drop.
func (r *Runtime) serveSubscription(w http.ResponseWriter, request *http.Request,
	message envelope) {
	controller := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	tools := wantsToolsListChanged(message.Params)
	send := func(payload []byte) bool {
		event := append(append([]byte("data: "), payload...), "\n\n"...)
		if _, err := w.Write(event); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	if !send(acknowledgment(message.ID, tools)) {
		return
	}
	r.streamChanges(request.Context(), message, tools, send, func() bool {
		_, err := w.Write([]byte(": keep-alive\n\n"))
		return err == nil && controller.Flush() == nil
	})
}

func (r *Runtime) streamChanges(ctx context.Context, message envelope, tools bool,
	send func([]byte) bool, keepAlive func() bool) {
	closeAt := closingTime(ctx)
	ticker := time.NewTicker(r.watchInterval)
	defer ticker.Stop()
	idle := time.NewTicker(keepAliveInterval)
	defer idle.Stop()
	last := r.server.Fingerprint()
	for {
		select {
		case <-ctx.Done():
			send(completion(message.ID))
			return
		case <-closeAt:
			send(completion(message.ID))
			return
		case <-idle.C:
			if !keepAlive() {
				return
			}
		case <-ticker.C:
			now := r.server.Fingerprint()
			if now == last {
				continue
			}
			last = now
			if tools && !send(listChanged(message.ID)) {
				return
			}
		}
	}
}

// closingTime fires shortly before ctx's deadline (never when it has
// none): a second before it, or a third of the time left when that is
// shorter.
func closingTime(ctx context.Context) <-chan time.Time {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil
	}
	left := time.Until(deadline)
	margin := time.Second
	if left < 3*margin {
		margin = left / 3
	}
	return time.After(left - margin)
}
