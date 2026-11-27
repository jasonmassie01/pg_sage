package selfmonitor

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// captureConn stands in for the connection the tagging writer sends
// through: it records every byte handed to it. With err set, each write
// takes the first accept bytes (none by default) and fails with err.
type captureConn struct {
	out    bytes.Buffer
	err    error
	accept int
	writes int
}

func (c *captureConn) Write(p []byte) (int, error) {
	c.writes++
	if c.err != nil {
		n := min(c.accept, len(p))
		c.out.Write(p[:n])
		return n, c.err
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

// writeAll writes stream to w in chunk-sized writes; each must report its
// whole input consumed, which is what pgx's frontend expects of a writer.
func writeAll(t *testing.T, w io.Writer, stream []byte, chunk int) {
	t.Helper()
	for len(stream) > 0 {
		n := min(chunk, len(stream))
		wrote, err := w.Write(stream[:n])
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		if wrote != n {
			t.Fatalf("Write returned %d for %d input bytes", wrote, n)
		}
		stream = stream[n:]
	}
}

func TestTagWriter_SimpleQueryGetsTag(t *testing.T) {
	cc := &captureConn{}
	writeAll(t, TagWriter(cc), append(startup(t),
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

func TestTagWriter_ParseKeepsNameAndParameterTypes(t *testing.T) {
	cc := &captureConn{}
	parse := &pgproto3.Parse{Name: "stmtcache_1", Query: "SELECT $1::int + $2",
		ParameterOIDs: []uint32{23, 20}}
	writeAll(t, TagWriter(cc), append(startup(t), encode(t, parse,
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
	writeAll(t, TagWriter(cc), append(startup(t),
		encode(t, &pgproto3.Query{String: sql})...), 1<<20)
	return decodeFrontend(t, cc.out.Bytes())[0].(*pgproto3.Query).String
}

// PostgreSQL 18's pg_stat_statements drops comments before a statement
// (it keeps the text from the first token), so the tag goes right after
// the first keyword, or after the opening parenthesis of a parenthesized
// query (a tag in front of it was dropped on 18). A leading pg_sage
// comment (with its component label) moves there instead of being
// repeated; one already there stays put.
func TestTagWriter_TagSitsAfterTheFirstKeyword(t *testing.T) {
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

func TestTagWriter_WhitespaceOnlyQueryUnchanged(t *testing.T) {
	if got := queryThroughTagger(t, " \n\t"); got != " \n\t" {
		t.Fatalf("whitespace query = %q, want it left alone", got)
	}
}

func TestTagWriter_EmptyQueryUnchanged(t *testing.T) {
	cc := &captureConn{}
	writeAll(t, TagWriter(cc), append(startup(t),
		encode(t, &pgproto3.Query{String: ""})...), 1<<20)
	if q := decodeFrontend(t, cc.out.Bytes())[0].(*pgproto3.Query); q.String != "" {
		t.Fatalf("empty query = %q, want it left empty", q.String)
	}
}

// A message split across writes is reassembled; each Write reports its
// whole input consumed, which is what pgx's frontend expects.
func TestTagWriter_SplitWritesMatchSingleWrite(t *testing.T) {
	stream := append(startup(t), encode(t,
		&pgproto3.Parse{Query: "SELECT $1::text"},
		&pgproto3.Bind{Parameters: [][]byte{[]byte("x")}},
		&pgproto3.Execute{},
		&pgproto3.Sync{},
		&pgproto3.Query{String: "SELECT 42"})...)
	whole := &captureConn{}
	writeAll(t, TagWriter(whole), stream, len(stream))
	for _, chunk := range []int{1, 3, 7, 64} {
		split := &captureConn{}
		writeAll(t, TagWriter(split), stream, chunk)
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

// Boundary: a message is forwarded only once all of it has arrived. A part
// shorter than the header (4 bytes for the untyped startup message, 5 for
// a typed one) or than the declared length is held, Write still reports it
// consumed, and the connection is not written to until the rest arrives.
func TestTagWriter_IncompleteMessageIsHeldUntilComplete(t *testing.T) {
	st := startup(t)
	query := encode(t, &pgproto3.Query{String: "SELECT 1"})
	for _, cut := range []int{1, 4, 5, len(query) - 1} {
		cc := &captureConn{}
		w := TagWriter(cc)
		writeAll(t, w, st[:3], 3)
		if cc.writes != 0 {
			t.Fatalf("cut %d: 3 bytes of the startup message were forwarded", cut)
		}
		writeAll(t, w, st[3:], len(st)-3)
		writeAll(t, w, query[:cut], cut)
		if cc.writes != 1 {
			t.Fatalf("cut %d: connection writes = %d, want 1 (the startup message)",
				cut, cc.writes)
		}
		writeAll(t, w, query[cut:], len(query)-cut)
		msgs := decodeFrontend(t, cc.out.Bytes())
		q, ok := msgs[0].(*pgproto3.Query)
		if cc.writes != 2 || len(msgs) != 1 || !ok || q.String != "SELECT /* pg_sage */ 1" {
			t.Fatalf("cut %d: writes = %d, messages = %#v, want one tagged Query",
				cut, cc.writes, msgs)
		}
	}
}

// Messages other than Parse and Query are passed through byte for byte,
// even when their payload happens to look like a query, and so is the
// startup message.
func TestTagWriter_OtherMessagesByteIdentical(t *testing.T) {
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
	writeAll(t, TagWriter(cc), stream, len(stream))
	if !bytes.Equal(cc.out.Bytes(), stream) {
		t.Fatal("non-query messages were modified")
	}
}

func TestTagWriter_WriteErrorPropagates(t *testing.T) {
	wantErr := errors.New("connection reset")
	cc := &captureConn{err: wantErr}
	n, err := TagWriter(cc).Write(append(startup(t),
		encode(t, &pgproto3.Query{String: "SELECT 1"})...))
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0 when nothing reached the connection", n)
	}
}

// A write the connection took only part of reports 1 byte written: not 0
// (pgx would take the message as unsent and safe to retry) and not all of
// it (pgx would miss the failure).
func TestTagWriter_PartialWriteIsNotReportedAsUnsent(t *testing.T) {
	wantErr := errors.New("broken pipe")
	cc := &captureConn{}
	w := TagWriter(cc)
	writeAll(t, w, startup(t), 1<<20)
	cc.err, cc.accept = wantErr, 3
	n, err := w.Write(encode(t, &pgproto3.Query{String: "SELECT 1"}))
	if !errors.Is(err, wantErr) || n != 1 {
		t.Fatalf("n = %d err = %v, want 1 and %v", n, err, wantErr)
	}
}

func TestTagWriter_CorruptLengthIsAnError(t *testing.T) {
	cc := &captureConn{}
	w := TagWriter(cc)
	writeAll(t, w, startup(t), 1<<20)
	// 'Q' with a length of 2: shorter than the length field itself.
	n, err := w.Write([]byte{'Q', 0, 0, 0, 2})
	if err == nil || !strings.Contains(err.Error(), "invalid message length 2") || n != 0 {
		t.Fatalf("n = %d err = %v, want 0 and a message-length error", n, err)
	}
	if cc.writes != 1 {
		t.Fatalf("connection writes = %d, want 1: a corrupt message is never sent", cc.writes)
	}
}

// Boundary: a declared length below 4 (the length field alone) or above
// maxMessageBytes is an error; 4 (an empty body, as Sync has) is sent and
// maxMessageBytes itself is accepted and held for its body.
func TestTagWriter_MessageLengthBounds(t *testing.T) {
	cases := []struct {
		size       uint32
		wantErr    bool
		wantWrites int
	}{
		{3, true, 1},
		{4, false, 2},
		{maxMessageBytes, false, 1},
		{maxMessageBytes + 1, true, 1},
	}
	for _, c := range cases {
		cc := &captureConn{}
		w := TagWriter(cc)
		writeAll(t, w, startup(t), 1<<20)
		header := binary.BigEndian.AppendUint32([]byte{'S'}, c.size)
		n, err := w.Write(header)
		want := fmt.Sprintf("invalid message length %d", c.size)
		switch {
		case c.wantErr && (err == nil || !strings.Contains(err.Error(), want) || n != 0):
			t.Errorf("size %d: n = %d err = %v, want 0 and %q", c.size, n, err, want)
		case !c.wantErr && (err != nil || n != len(header)):
			t.Errorf("size %d: n = %d err = %v, want %d and no error",
				c.size, n, err, len(header))
		case cc.writes != c.wantWrites:
			t.Errorf("size %d: connection writes = %d, want %d", c.size, cc.writes,
				c.wantWrites)
		}
	}
}

func TestTagWriter_NilWriterIsNil(t *testing.T) {
	if TagWriter(nil) != nil {
		t.Fatal("TagWriter(nil) must stay nil")
	}
}

// TagWriter wraps exactly the writer it is given, starting before the
// startup message.
func TestTagWriter_WrapsTheGivenWriter(t *testing.T) {
	cc := &captureConn{}
	tw, ok := TagWriter(cc).(*tagWriter)
	if !ok || tw.w != cc || tw.started || tw.pending != nil {
		t.Fatalf("TagWriter(cc) = %#v, want a fresh *tagWriter around cc", tw)
	}
}

// Concurrent access: every TagWriter keeps its own state (startup seen,
// held tail), so the writers of connections used at once never mix their
// bytes; under -race this also finds any shared mutable state.
func TestTagWriter_IndependentWritersConcurrently(t *testing.T) {
	const writers = 16
	outs := make([]*captureConn, writers)
	streams := make([][]byte, writers)
	for i := range writers {
		outs[i] = &captureConn{}
		streams[i] = append(startup(t), encode(t,
			&pgproto3.Query{String: fmt.Sprintf("SELECT %d", i)})...)
	}
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		w, stream := TagWriter(outs[i]), streams[i]
		wg.Go(func() {
			for len(stream) > 0 {
				n := min(3, len(stream))
				if _, err := w.Write(stream[:n]); err != nil {
					errs <- fmt.Errorf("writer %d: %w", i, err)
					return
				}
				stream = stream[n:]
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for i, cc := range outs {
		want := fmt.Sprintf("SELECT /* pg_sage */ %d", i)
		if q := decodeFrontend(t, cc.out.Bytes())[0].(*pgproto3.Query); q.String != want {
			t.Errorf("writer %d sent %q, want %q", i, q.String, want)
		}
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
