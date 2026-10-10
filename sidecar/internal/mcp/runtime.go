package mcp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// defaultWatchInterval is how often a runtime checks whether the tool
// list changed (a database joined or left the fleet).
const defaultWatchInterval = 15 * time.Second

// maxStdioLine bounds one JSON-RPC message on stdio.
const maxStdioLine = 4 << 20

type Runtime struct {
	transport     string
	server        *Server
	input         io.Reader
	output        io.Writer
	httpHandler   http.Handler
	watchInterval time.Duration
	// stdioPrincipalID is the agent principal mcp.stdio_principal names
	// ("" when none is configured).
	stdioPrincipalID string
}

func NewRuntime(
	cfg config.MCPConfig, server *Server, input io.Reader, output io.Writer,
) (*Runtime, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	result := &Runtime{transport: cfg.Transport, server: server, input: input,
		output: output, watchInterval: defaultWatchInterval}
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

// BindStdioPrincipal names the agent principal ("agp_…") the stdio client
// acts as; its tools then carry that principal. The caller also binds the
// principal's agentguard identity on the context it passes to Serve.
func (r *Runtime) BindStdioPrincipal(principalID string) {
	r.stdioPrincipalID = principalID
}

// SetWatchInterval sets how often the tool list is checked for changes.
func (r *Runtime) SetWatchInterval(d time.Duration) {
	if d > 0 {
		r.watchInterval = d
	}
}

// Serve runs the stdio transport until the input ends (nil) or ctx is
// cancelled (ctx.Err(), after closing open subscriptions gracefully).
func (r *Runtime) Serve(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.transport != "stdio" {
		return fmt.Errorf("Serve is only valid for stdio transport")
	}
	p := stdioPrincipal
	p.PrincipalID = r.stdioPrincipalID
	ctx = WithPrincipal(ctx, p)
	session := newStdioSession(r.server, r.output)
	// The baseline is taken before any input is read: a change made once a
	// request is answered must not fall into a baseline the watcher
	// goroutine takes whenever it first runs (and be lost).
	baseline := r.server.Fingerprint()
	lines, readErr := readLines(ctx, r.input)
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go r.watch(watchCtx, baseline, session.toolsChanged)
	for {
		select {
		case <-ctx.Done():
			session.closeSubscriptions()
			return ctx.Err()
		case line, ok := <-lines:
			if !ok {
				return <-readErr
			}
			if err := session.handle(ctx, line); err != nil {
				return err
			}
		}
	}
}

// readLines scans newline-delimited messages on its own goroutine, so a
// blocked read never delays shutdown.
func readLines(ctx context.Context, input io.Reader) (<-chan []byte, <-chan error) {
	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 64<<10), maxStdioLine)
		for scanner.Scan() {
			select {
			case lines <- append([]byte(nil), scanner.Bytes()...):
			case <-ctx.Done():
				return
			}
		}
		readErr <- scanner.Err()
	}()
	return lines, readErr
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
