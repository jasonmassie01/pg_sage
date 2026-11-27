package selfmonitor

import (
	"encoding/binary"
	"fmt"
	"io"
)

// maxMessageBytes bounds one protocol message the wrapper will buffer; it
// is far above anything pgx sends and only stops a corrupt length.
const maxMessageBytes = 1 << 30

// TagWriter wraps the writer pgx's protocol frontend sends through and
// places StatementTag after the first keyword of every statement of a
// Query message (placeTags) and of a Parse message's statement
// (placeTag). All other bytes pass through unchanged. ConfigureConn
// installs it through the connection's BuildFrontend, so the tag covers
// every statement pgx sends through its frontend, its own included.
//
// It wraps the frontend's writer, not the network connection: pgx enables
// SCRAM channel binding only when the connection it holds is a *tls.Conn,
// so a wrapped connection refused channel_binding=require and silently
// dropped to SCRAM-SHA-256 under the default prefer. One pgx path writes
// past the frontend: a batch sent in QueryExecModeExec (pgconn ExecBatch)
// goes out untagged. TestBatchSQLCensus (internal/selfcost) checks every
// SQL text a non-test Queue call can receive with the leading comments
// PostgreSQL 18 drops removed: IsStatementText must still recognize it (a
// pg_sage tag after its first keyword, or a sage object), and a tag it
// carries must keep its component. SQL the census cannot resolve must be
// on its allow-list with a reason.
func TagWriter(w io.Writer) io.Writer {
	if w == nil {
		return nil
	}
	return &tagWriter{w: w}
}

type tagWriter struct {
	w       io.Writer
	started bool   // the untyped startup message has been forwarded
	pending []byte // an incomplete message carried to the next Write
}

// Write consumes all of p. Complete messages are forwarded (rewritten
// when they carry a statement); an incomplete tail is held until the next
// Write completes it.
func (c *tagWriter) Write(p []byte) (int, error) {
	buf := p
	if len(c.pending) > 0 {
		buf = append(c.pending, p...)
		c.pending = nil
	}
	out, rest, err := c.rewrite(buf)
	if err != nil {
		return 0, err
	}
	if len(rest) > 0 {
		c.pending = append([]byte(nil), rest...)
	}
	if len(out) == 0 {
		return len(p), nil
	}
	n, err := c.w.Write(out)
	if err != nil {
		if n == 0 {
			return 0, err
		}
		return 1, err // partially sent: not safe to retry
	}
	return len(p), nil
}

// rewrite returns the bytes to send for every complete message in buf and
// the incomplete remainder.
func (c *tagWriter) rewrite(buf []byte) ([]byte, []byte, error) {
	var out []byte
	for len(buf) > 0 {
		if !c.started && buf[0] != 0 {
			// A frontend rebuilt mid-session (pgconn.Construct after a
			// Hijack) starts at a typed message: no startup message comes.
			// A startup message's length is far below 2^24, so its first
			// byte is 0, and no message type is 0.
			c.started = true
		}
		header := 5
		if !c.started {
			header = 4 // StartupMessage: no type byte
		}
		if len(buf) < header {
			return out, buf, nil
		}
		size := int(binary.BigEndian.Uint32(buf[header-4 : header]))
		if size < 4 || size > maxMessageBytes {
			return nil, nil, fmt.Errorf("pg_sage statement tagger: invalid message "+
				"length %d", size)
		}
		total := header - 4 + size
		if len(buf) < total {
			return out, buf, nil
		}
		msg := buf[:total]
		if !c.started {
			c.started = true
		} else {
			msg = tagMessage(msg)
		}
		out = append(out, msg...)
		buf = buf[total:]
	}
	return out, nil, nil
}

// tagMessage returns msg with its statements' tags placed when it is a
// Query or Parse message that needs them; otherwise msg itself.
func tagMessage(msg []byte) []byte {
	body := msg[5:]
	switch msg[0] {
	case 'Q':
		query, ok := cString(body)
		if !ok {
			return msg
		}
		tagged := placeTags(query)
		if tagged == query {
			return msg
		}
		return buildMessage('Q', nil, tagged, body[len(query)+1:])
	case 'P':
		name, ok := cString(body)
		if !ok {
			return msg
		}
		rest := body[len(name)+1:]
		query, ok := cString(rest)
		if !ok {
			return msg
		}
		tagged := placeTag(query)
		if tagged == query {
			return msg
		}
		return buildMessage('P', body[:len(name)+1], tagged, rest[len(query)+1:])
	default:
		return msg
	}
}

// buildMessage encodes type, prefix, the NUL-terminated query and suffix
// with a fresh length.
func buildMessage(typ byte, prefix []byte, query string, suffix []byte) []byte {
	size := 4 + len(prefix) + len(query) + 1 + len(suffix)
	out := make([]byte, 0, 1+size)
	out = append(out, typ)
	out = binary.BigEndian.AppendUint32(out, uint32(size))
	out = append(out, prefix...)
	out = append(out, query...)
	out = append(out, 0)
	return append(out, suffix...)
}

// cString reads a NUL-terminated string from the start of b.
func cString(b []byte) (string, bool) {
	for i, ch := range b {
		if ch == 0 {
			return string(b[:i]), true
		}
	}
	return "", false
}
