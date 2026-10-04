package selfmonitor

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

// maxMessageBytes bounds one protocol message the wrapper will buffer; it
// is far above anything pgx sends and only stops a corrupt length.
const maxMessageBytes = 1 << 30

// TagConn wraps the connection pgx speaks the PostgreSQL protocol over
// (after TLS) and places StatementTag after the first keyword of every
// statement of a Query message (placeTags) and of a Parse message's
// statement (placeTag). All other bytes pass through unchanged. It is installed through ConfigurePool, so the tag covers
// every statement from every code path, including pgx's own.
func TagConn(conn net.Conn) net.Conn {
	if conn == nil {
		return nil
	}
	return &tagConn{Conn: conn}
}

type tagConn struct {
	net.Conn
	started bool   // the untyped startup message has been forwarded
	pending []byte // an incomplete message carried to the next Write
}

// Write consumes all of p. Complete messages are forwarded (rewritten
// when they carry a statement); an incomplete tail is held until the next
// Write completes it.
func (c *tagConn) Write(p []byte) (int, error) {
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
	n, err := c.Conn.Write(out)
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
func (c *tagConn) rewrite(buf []byte) ([]byte, []byte, error) {
	var out []byte
	for len(buf) > 0 {
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

// errNilConn is returned when pgx hands the hook no connection.
var errNilConn = errors.New("pg_sage statement tagger: no network connection")
