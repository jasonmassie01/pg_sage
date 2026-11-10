package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Concurrent tools/call on stdio (mcp-04): each call runs on its own
// goroutine under the session's call timeout, at most maxStdioInflight at
// once; notifications/cancelled cancels one by id.

// maxStdioInflight caps the tools/call requests one stdio session runs at
// once. The reader waits for a free slot, so a client that floods calls is
// slowed down rather than queued without bound.
const maxStdioInflight = 4

// inflightCall is one running tools/call. cancelled (guarded by the
// session's mu) records that the client cancelled it, so no response is
// written for it.
type inflightCall struct {
	cancel    context.CancelFunc
	cancelled bool
}

// run handles lines until the input ends, a read or write fails or ctx is
// cancelled, then waits for every in-flight call before returning. At EOF
// the calls run to completion and are answered; when the session ended on
// an error its in-flight calls are cancelled first, as on shutdown, since
// the session that would answer them is gone.
func (s *stdioSession) run(ctx context.Context, lines <-chan stdioLine,
	readErr <-chan error) error {
	err := s.loop(ctx, lines, readErr)
	if err != nil {
		s.cancelInflight()
	}
	s.calls.Wait()
	if ctx.Err() != nil {
		s.closeSubscriptions()
		return ctx.Err()
	}
	if err == nil {
		select {
		case err = <-s.failed:
		default:
		}
	}
	return err
}

func (s *stdioSession) loop(ctx context.Context, lines <-chan stdioLine,
	readErr <-chan error) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-s.failed:
			return err
		case line, ok := <-lines:
			if !ok {
				return <-readErr
			}
			if err := s.handle(ctx, line); err != nil {
				return err
			}
		}
	}
}

// cancelInflight cancels every running tools/call. The loop has returned,
// so no call starts after it.
func (s *stdioSession) cancelInflight() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, call := range s.inflight {
		call.cancel()
	}
}

// startCall runs one tools/call on its own goroutine once a slot is free.
// An id already in flight is refused: a cancel could not tell them apart.
func (s *stdioSession) startCall(ctx context.Context, id json.RawMessage,
	line []byte) error {
	key := string(id)
	s.mu.Lock()
	_, busy := s.inflight[key]
	s.mu.Unlock()
	if busy {
		return s.write(encodeResponse(rpcResponse{JSONRPC: "2.0", ID: id,
			Error: failure(codeInvalidRequest, "request id "+key+" is already in flight")}))
	}
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	call := &inflightCall{cancel: cancel}
	s.mu.Lock()
	s.inflight[key] = call
	s.mu.Unlock()
	s.calls.Add(1)
	go s.runCall(callCtx, call, id, line)
	return nil
}

// runCall answers one call. A call the client cancelled gets no response
// and one past its deadline is answered -32800, unless it succeeded anyway:
// a result that happened is never hidden from the client.
func (s *stdioSession) runCall(ctx context.Context, call *inflightCall,
	id json.RawMessage, line []byte) {
	defer s.calls.Done()
	defer func() { <-s.slots }()
	response := s.server.Handle(ctx, line)
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	call.cancel()
	s.mu.Lock()
	delete(s.inflight, string(id))
	cancelled := call.cancelled
	s.mu.Unlock()
	if !succeeded(response) {
		if cancelled {
			return
		}
		if timedOut {
			response = encodeResponse(rpcResponse{JSONRPC: "2.0", ID: id,
				Error: failure(codeCancelled, fmt.Sprintf("tools/call timed out after %s "+
					"(mcp.call_timeout_seconds)", s.timeout))})
		}
	}
	if response == nil {
		return
	}
	if err := s.write(response); err != nil {
		select {
		case s.failed <- err:
		default:
		}
	}
}

// succeeded reports whether a tools/call response is a result that is not
// a tool error.
func succeeded(response []byte) bool {
	var reply struct {
		Error  json.RawMessage `json:"error"`
		Result *struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	return json.Unmarshal(response, &reply) == nil && len(reply.Error) == 0 &&
		reply.Result != nil && !reply.Result.IsError
}
