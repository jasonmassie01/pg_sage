package selfmonitor

import (
	"bytes"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// captureConn records what the tagging wrapper hands the network.
type captureConn struct {
	net.Conn
	out    bytes.Buffer
	err    error
	writes int
}

func (c *captureConn) Write(p []byte) (int, error) {
	c.writes++
	if c.err != nil {
		return 0, c.err
	}
	return c.out.Write(p)
}

func encode(t *testing.T, msgs ...pgproto3.FrontendMessage) []byte {
	t.Helper()
	var buf []byte
	for _, m := range msgs {
		var err error
		buf, err = m.Encode(buf)
		if err != nil {
			t.Fatalf("encode %T: %v", m, err)
		}
	}
	return buf
}

func startup(t *testing.T) []byte {
	t.Helper()
	return encode(t, &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "u", "database": "d"},
	})
}

// decodeFrontend reads what a server would read: the startup message,
// then typed messages until the stream ends.
func decodeFrontend(t *testing.T, stream []byte) []pgproto3.FrontendMessage {
	t.Helper()
	be := pgproto3.NewBackend(bytes.NewReader(stream), nil)
	first, err := be.ReceiveStartupMessage()
	if err != nil {
		t.Fatalf("receive startup: %v", err)
	}
	if sm, ok := first.(*pgproto3.StartupMessage); !ok || sm.Parameters["user"] != "u" {
		t.Fatalf("startup message changed: %#v", first)
	}
	var out []pgproto3.FrontendMessage
	for {
		msg, err := be.Receive()
		if err != nil {
			return out
		}
		out = append(out, copyFrontend(t, msg))
	}
}

// copyFrontend detaches a decoded message from the backend's buffer.
func copyFrontend(t *testing.T, msg pgproto3.FrontendMessage) pgproto3.FrontendMessage {
	t.Helper()
	switch m := msg.(type) {
	case *pgproto3.Query:
		return &pgproto3.Query{String: m.String}
	case *pgproto3.Parse:
		return &pgproto3.Parse{Name: m.Name, Query: m.Query,
			ParameterOIDs: append([]uint32(nil), m.ParameterOIDs...)}
	case *pgproto3.CopyData:
		return &pgproto3.CopyData{Data: append([]byte(nil), m.Data...)}
	default:
		return msg
	}
}

func writeAll(t *testing.T, conn net.Conn, stream []byte, chunk int) {
	t.Helper()
	for len(stream) > 0 {
		n := min(chunk, len(stream))
		wrote, err := conn.Write(stream[:n])
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		if wrote != n {
			t.Fatalf("Write returned %d for %d input bytes", wrote, n)
		}
		stream = stream[n:]
	}
}

func TestTagConn_SimpleQueryGetsTag(t *testing.T) {
	cc := &captureConn{}
	conn := TagConn(cc)
	writeAll(t, conn, append(startup(t),
		encode(t, &pgproto3.Query{String: "SELECT 1"})...), 1<<20)
	msgs := decodeFrontend(t, cc.out.Bytes())
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	q, ok := msgs[0].(*pgproto3.Query)
	if !ok || q.String != "SELECT /* pg_sage */ 1" {
		t.Fatalf("query = %#v, want the tag after SELECT", msgs[0])
	}
}

func TestTagConn_ParseKeepsNameAndParameterTypes(t *testing.T) {
	cc := &captureConn{}
	conn := TagConn(cc)
	parse := &pgproto3.Parse{Name: "stmtcache_1", Query: "SELECT $1::int + $2",
		ParameterOIDs: []uint32{23, 20}}
	writeAll(t, conn, append(startup(t), encode(t, parse,
		&pgproto3.Describe{ObjectType: 'S', Name: "stmtcache_1"},
		&pgproto3.Sync{})...), 1<<20)
	msgs := decodeFrontend(t, cc.out.Bytes())
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (Parse, Describe, Sync)", len(msgs))
	}
	got, ok := msgs[0].(*pgproto3.Parse)
	if !ok {
		t.Fatalf("first message = %T, want Parse", msgs[0])
	}
	if got.Name != "stmtcache_1" || got.Query != "SELECT /* pg_sage */ $1::int + $2" {
		t.Fatalf("parse = %+v", got)
	}
	if len(got.ParameterOIDs) != 2 || got.ParameterOIDs[0] != 23 || got.ParameterOIDs[1] != 20 {
		t.Fatalf("parameter OIDs = %v, want [23 20]", got.ParameterOIDs)
	}
	if d, ok := msgs[1].(*pgproto3.Describe); !ok || d.Name != "stmtcache_1" {
		t.Fatalf("describe changed: %#v", msgs[1])
	}
}

// queryThroughTagger sends sql as a simple Query and returns what reaches
// the server.
func queryThroughTagger(t *testing.T, sql string) string {
	t.Helper()
	cc := &captureConn{}
	writeAll(t, TagConn(cc), append(startup(t),
		encode(t, &pgproto3.Query{String: sql})...), 1<<20)
	return decodeFrontend(t, cc.out.Bytes())[0].(*pgproto3.Query).String
}

// PostgreSQL 18's pg_stat_statements drops comments before a statement
// (it keeps the text from the first token), so the tag goes right after
// the first keyword, or after the opening parenthesis of a parenthesized
// query (a tag in front of it was dropped on 18). A leading pg_sage
// comment (with its component label) moves there instead of being
// repeated; one already there stays put.
func TestTagConn_TagSitsAfterTheFirstKeyword(t *testing.T) {
	cases := map[string]string{
		"/* pg_sage */SELECT 1":  "SELECT /* pg_sage */ 1",
		"/* pg_sage */ SELECT 1": "SELECT /* pg_sage */ 1",
		"  \n\t/* pg_sage sre:lock_chains v1 */\nSELECT 2": "  \n\tSELECT " +
			"/* pg_sage sre:lock_chains v1 */ 2",
		"/* pg_sage:collector */ SELECT 3": "SELECT /* pg_sage:collector */ 3",
		"SELECT /* pg_sage */ 1":           "SELECT /* pg_sage */ 1",
		"WITH /* pg_sage sre:x v1 */ a AS (SELECT 1) SELECT * FROM a": "WITH " +
			"/* pg_sage sre:x v1 */ a AS (SELECT 1) SELECT * FROM a",
		"/* app:report */ SELECT 1":       "/* app:report */ SELECT /* pg_sage */ 1",
		"-- note\nselect 1":               "-- note\nselect /* pg_sage */ 1",
		"/* a /* nested */ b */ SELECT 1": "/* a /* nested */ b */ SELECT /* pg_sage */ 1",
		"SELECT*FROM t":                   "SELECT /* pg_sage */ *FROM t",
		"BEGIN":                           "BEGIN /* pg_sage */",
		"(SELECT 1) UNION SELECT 2":       "( /* pg_sage */ SELECT 1) UNION SELECT 2",
		"/* pg_sage */ (SELECT 1)":        "( /* pg_sage */ SELECT 1)",
		"((SELECT 1))":                    "( /* pg_sage */ (SELECT 1))",
		"( /* pg_sage */ SELECT 1)":       "( /* pg_sage */ SELECT 1)",
		"$1":                              "/* pg_sage */ $1",
		"/* unterminated":                 "/* unterminated",
	}
	for in, want := range cases {
		if got := queryThroughTagger(t, in); got != want {
			t.Errorf("%q\n  got  %q\n  want %q", in, got, want)
		}
	}
}

func TestTagConn_WhitespaceOnlyQueryUnchanged(t *testing.T) {
	if got := queryThroughTagger(t, " \n\t"); got != " \n\t" {
		t.Fatalf("whitespace query = %q, want it left alone", got)
	}
}

func TestTagConn_EmptyQueryUnchanged(t *testing.T) {
	cc := &captureConn{}
	writeAll(t, TagConn(cc), append(startup(t),
		encode(t, &pgproto3.Query{String: ""})...), 1<<20)
	if q := decodeFrontend(t, cc.out.Bytes())[0].(*pgproto3.Query); q.String != "" {
		t.Fatalf("empty query = %q, want it left empty", q.String)
	}
}

// A message split across writes is reassembled; each Write reports its
// whole input consumed, which is what pgx expects from a net.Conn.
func TestTagConn_SplitWritesMatchSingleWrite(t *testing.T) {
	stream := append(startup(t), encode(t,
		&pgproto3.Parse{Query: "SELECT $1::text"},
		&pgproto3.Bind{Parameters: [][]byte{[]byte("x")}},
		&pgproto3.Execute{},
		&pgproto3.Sync{},
		&pgproto3.Query{String: "SELECT 42"})...)
	whole := &captureConn{}
	writeAll(t, TagConn(whole), stream, len(stream))
	for _, chunk := range []int{1, 3, 7, 64} {
		split := &captureConn{}
		writeAll(t, TagConn(split), stream, chunk)
		if !bytes.Equal(split.out.Bytes(), whole.out.Bytes()) {
			t.Fatalf("chunk %d: split output differs from single write", chunk)
		}
	}
	msgs := decodeFrontend(t, whole.out.Bytes())
	if p := msgs[0].(*pgproto3.Parse); p.Query != "SELECT /* pg_sage */ $1::text" {
		t.Fatalf("parse query = %q", p.Query)
	}
	if q := msgs[4].(*pgproto3.Query); q.String != "SELECT /* pg_sage */ 42" {
		t.Fatalf("query = %q", q.String)
	}
}

// Messages other than Parse and Query are passed through byte for byte,
// even when their payload happens to look like a query.
func TestTagConn_OtherMessagesByteIdentical(t *testing.T) {
	others := encode(t,
		&pgproto3.Bind{DestinationPortal: "p", PreparedStatement: "s",
			Parameters: [][]byte{[]byte("Q\x00\x00\x00\x09SELECT")}},
		&pgproto3.Describe{ObjectType: 'P', Name: "p"},
		&pgproto3.Execute{Portal: "p", MaxRows: 10},
		&pgproto3.CopyData{Data: []byte("Q\x00\x00\x00\x0dSELECT 1\x00")},
		&pgproto3.CopyDone{},
		&pgproto3.PasswordMessage{Password: "not-a-query"},
		&pgproto3.Sync{},
		&pgproto3.Terminate{})
	stream := append(startup(t), others...)
	cc := &captureConn{}
	writeAll(t, TagConn(cc), stream, len(stream))
	if !bytes.Equal(cc.out.Bytes(), stream) {
		t.Fatal("non-query messages were modified")
	}
}

func TestTagConn_WriteErrorPropagates(t *testing.T) {
	wantErr := errors.New("connection reset")
	cc := &captureConn{err: wantErr}
	conn := TagConn(cc)
	n, err := conn.Write(append(startup(t), encode(t, &pgproto3.Query{String: "SELECT 1"})...))
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0 when nothing reached the network", n)
	}
}

func TestTagConn_CorruptLengthIsAnError(t *testing.T) {
	cc := &captureConn{}
	conn := TagConn(cc)
	writeAll(t, conn, startup(t), 1<<20)
	// 'Q' with a length of 2: shorter than the length field itself.
	_, err := conn.Write([]byte{'Q', 0, 0, 0, 2})
	if err == nil || !strings.Contains(err.Error(), "length") {
		t.Fatalf("err = %v, want a message-length error", err)
	}
}

func TestTagConn_NilConnIsNil(t *testing.T) {
	if TagConn(nil) != nil {
		t.Fatal("TagConn(nil) must stay nil")
	}
}

func TestIsTagged(t *testing.T) {
	cases := map[string]bool{
		"/* pg_sage */ SELECT 1":            true,
		"\n  /* pg_sage sre:x v1 */ SELECT": true,
		"SELECT 1":                          false,
		"/* app */ SELECT 1":                false,
		"SELECT /* pg_sage */ 1":            true,
		"SELECT $1 /*pg_sage*/":             true,
		"":                                  false,
		"SELECT 1 -- pg_sage":               false,
	}
	for sql, want := range cases {
		if got := IsTagged(sql); got != want {
			t.Errorf("IsTagged(%q) = %v, want %v", sql, got, want)
		}
	}
}
