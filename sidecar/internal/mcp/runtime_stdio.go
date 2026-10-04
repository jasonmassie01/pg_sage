package mcp

import (
	"context"
	"encoding/json"
	"io"
	"sync"
)

// stdioSession is one stdio connection: a serialized writer, whether the
// client opened a handshake-era session (it then gets list_changed
// notifications) and its open subscriptions/listen streams.
type stdioSession struct {
	server *Server
	mu     sync.Mutex
	out    io.Writer
	legacy bool
	subs   map[string]subscription
}

// subscription is one subscriptions/listen request: its id (raw JSON) and
// whether it asked for tools/list_changed.
type subscription struct {
	id    json.RawMessage
	tools bool
}

func newStdioSession(server *Server, out io.Writer) *stdioSession {
	return &stdioSession{server: server, out: out, subs: map[string]subscription{}}
}

func (s *stdioSession) write(message []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.out.Write(append(message, '\n'))
	return err
}

type envelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func (s *stdioSession) handle(ctx context.Context, line []byte) error {
	var message envelope
	_ = json.Unmarshal(line, &message)
	switch message.Method {
	case "subscriptions/listen":
		if len(message.ID) > 0 {
			return s.listen(message)
		}
		return nil
	case "notifications/cancelled":
		s.cancel(message.Params)
		return nil
	}
	response := s.server.Handle(ctx, line)
	if response == nil {
		return nil
	}
	if message.Method == "initialize" {
		s.mu.Lock()
		s.legacy = true
		s.mu.Unlock()
	}
	return s.write(response)
}

func (s *stdioSession) listen(message envelope) error {
	meta := parseMeta(message.Params)
	if meta.modern && !supportedVersion(meta.version) {
		return s.write(encodeResponse(rpcResponse{JSONRPC: "2.0", ID: message.ID,
			Error: unsupportedVersion(meta.version)}))
	}
	tools := wantsToolsListChanged(message.Params)
	s.mu.Lock()
	s.subs[string(message.ID)] = subscription{id: message.ID, tools: tools}
	s.mu.Unlock()
	return s.write(acknowledgment(message.ID, tools))
}

func wantsToolsListChanged(params json.RawMessage) bool {
	var request struct {
		Notifications struct {
			ToolsListChanged bool `json:"toolsListChanged"`
		} `json:"notifications"`
	}
	return json.Unmarshal(params, &request) == nil && request.Notifications.ToolsListChanged
}

func acknowledgment(id json.RawMessage, tools bool) []byte {
	agreed := map[string]any{}
	if tools {
		agreed["toolsListChanged"] = true
	}
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0",
		"method": "notifications/subscriptions/acknowledged",
		"params": map[string]any{"_meta": map[string]any{metaSubscriptionKey: id},
			"notifications": agreed}})
	return raw
}

func listChanged(id json.RawMessage) []byte {
	message := map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"}
	if id != nil {
		message["params"] = map[string]any{"_meta": map[string]any{metaSubscriptionKey: id}}
	}
	raw, _ := json.Marshal(message)
	return raw
}

func completion(id json.RawMessage) []byte {
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id,
		"result": map[string]any{"resultType": "complete",
			"_meta": map[string]any{metaSubscriptionKey: id}}})
	return raw
}

func (s *stdioSession) cancel(params json.RawMessage) {
	var request struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(params, &request) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subs, string(request.RequestID))
}

// toolsChanged notifies the handshake-era session and every subscription
// that asked for tool list changes.
func (s *stdioSession) toolsChanged() {
	s.mu.Lock()
	legacy := s.legacy
	var ids []json.RawMessage
	for _, sub := range s.subs {
		if sub.tools {
			ids = append(ids, sub.id)
		}
	}
	s.mu.Unlock()
	if legacy {
		_ = s.write(listChanged(nil))
	}
	for _, id := range ids {
		_ = s.write(listChanged(id))
	}
}

// closeSubscriptions ends every open subscription with its completion
// result (a graceful end, as opposed to a dropped stream).
func (s *stdioSession) closeSubscriptions() {
	s.mu.Lock()
	subs := s.subs
	s.subs = map[string]subscription{}
	s.mu.Unlock()
	for _, sub := range subs {
		_ = s.write(completion(sub.id))
	}
}
