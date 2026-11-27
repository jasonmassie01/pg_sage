package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// defaultWatchInterval is how often a runtime checks whether the tool
// list changed (a database joined or left the fleet).
const defaultWatchInterval = 15 * time.Second

// maxStdioLine bounds one JSON-RPC message on stdio (line terminator
// excluded). A longer line is answered -32600 and skipped.
const maxStdioLine = 4 << 20

// stdioReadBuffer is the stdio reader's buffer; a longer line is read in
// pieces up to maxStdioLine.
const stdioReadBuffer = 64 << 10

type Runtime struct {
	transport     string
	server        *Server
	input         io.Reader
	output        io.Writer
	httpHandler   http.Handler
	watchInterval time.Duration
	// callTimeout bounds one stdio tools/call (mcp.call_timeout_seconds).
	callTimeout time.Duration
	// stdioPrincipalID is the agent principal mcp.stdio_principal names
	// ("" keeps the unbound stdio agent).
	stdioPrincipalID string
}

func NewRuntime(
	cfg config.MCPConfig, server *Server, input io.Reader, output io.Writer,
) (*Runtime, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	result := &Runtime{transport: cfg.Transport, server: server, input: input,
		output: output, watchInterval: defaultWatchInterval,
		callTimeout: cfg.CallTimeout()}
	switch cfg.Transport {
	case "stdio":
		if input == nil || output == nil {
			return nil, fmt.Errorf("stdio transport requires streams")
		}
	case "http":
		result.httpHandler = http.HandlerFunc(result.serveHTTP)
	default:
		return nil, fmt.Errorf("unsupported MCP transport %q", cfg.Transport)
	}
	return result, nil
}

func (r *Runtime) Transport() string         { return r.transport }
func (r *Runtime) HTTPHandler() http.Handler { return r.httpHandler }

// SetStdioPrincipal binds the stdio client to an agent principal id
// (mcp.stdio_principal, resolved at startup); "" keeps it unbound.
func (r *Runtime) SetStdioPrincipal(principalID string) {
	r.stdioPrincipalID = principalID
}

// SetWatchInterval sets how often the tool list is checked for changes.
func (r *Runtime) SetWatchInterval(d time.Duration) {
	if d > 0 {
		r.watchInterval = d
	}
}

// SetCallTimeout sets how long one stdio tools/call may run; d <= 0 keeps
// the configured timeout.
func (r *Runtime) SetCallTimeout(d time.Duration) {
	if d > 0 {
		r.callTimeout = d
	}
}

// Serve runs the stdio transport until the input ends (nil), a read or
// write fails (that error) or ctx is cancelled (ctx.Err(), after closing
// open subscriptions gracefully). It always waits for in-flight tool calls,
// cancelling them first unless the input simply ended.
func (r *Runtime) Serve(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.transport != "stdio" {
		return fmt.Errorf("Serve is only valid for stdio transport")
	}
	ctx = WithPrincipal(ctx, StdioPrincipalFor(r.stdioPrincipalID))
	session := newStdioSession(r.server, r.output, r.callTimeout)
	// The baseline is taken before any input is read: a change made once a
	// request is answered must not fall into a baseline the watcher
	// goroutine takes whenever it first runs (and be lost).
	baseline := r.server.Fingerprint()
	readCtx, stopRead := context.WithCancel(ctx)
	defer stopRead()
	lines, readErr := readLines(readCtx, r.input)
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go r.watch(watchCtx, baseline, session.toolsChanged)
	return session.run(ctx, lines, readErr)
}

// stdioLine is one message read from stdin, or the marker of a line longer
// than maxStdioLine (its bytes are discarded).
type stdioLine struct {
	data      []byte
	oversized bool
}

// readLines reads newline-delimited messages on its own goroutine, so a
// blocked read never delays shutdown. readErr carries nil at EOF, the read
// error otherwise, and is written before lines closes.
func readLines(ctx context.Context, input io.Reader) (<-chan stdioLine, <-chan error) {
	lines := make(chan stdioLine)
	readErr := make(chan error, 1)
	go func() {
		defer close(lines)
		reader := bufio.NewReaderSize(input, stdioReadBuffer)
		for {
			data, oversized, err := readBoundedLine(reader, maxStdioLine)
			if errors.Is(err, io.EOF) {
				readErr <- nil
				return
			}
			if err != nil {
				readErr <- fmt.Errorf("mcp stdio: read stdin: %w", err)
				return
			}
			select {
			case lines <- stdioLine{data: data, oversized: oversized}:
			case <-ctx.Done():
				readErr <- ctx.Err()
				return
			}
		}
	}()
	return lines, readErr
}

// readBoundedLine reads one newline-terminated message of at most limit
// bytes, terminator excluded. A longer line is discarded up to its newline
// and reported as oversized; a last line without a newline is returned,
// and io.EOF comes only once no bytes remain.
func readBoundedLine(reader *bufio.Reader, limit int) ([]byte, bool, error) {
	var line []byte
	oversized := false
	for {
		chunk, err := reader.ReadSlice('\n')
		if !oversized && len(line)+len(chunk) > limit+len("\r\n") {
			oversized, line = true, nil
		} else if !oversized {
			line = append(line, chunk...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && (oversized || len(line) > 0) {
			err = nil
		}
		if err != nil {
			return nil, false, err
		}
		return trimLine(line, limit, oversized)
	}
}

// trimLine drops the line terminator (LF or CRLF) and applies the limit.
func trimLine(line []byte, limit int, oversized bool) ([]byte, bool, error) {
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	if oversized || len(line) > limit {
		return nil, true, nil
	}
	return line, false, nil
}

// watch calls changed whenever the server's tool list fingerprint moves
// from last.
func (r *Runtime) watch(ctx context.Context, last string, changed func()) {
	ticker := time.NewTicker(r.watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if now := r.server.Fingerprint(); now != last {
				last = now
				changed()
			}
		}
	}
}
