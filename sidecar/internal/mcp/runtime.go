package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/pg-sage/sidecar/internal/config"
)

type Runtime struct {
	transport   string
	server      *Server
	input       io.Reader
	output      io.Writer
	httpHandler http.Handler
}

func NewRuntime(
	cfg config.MCPConfig, server *Server, input io.Reader, output io.Writer,
) (*Runtime, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	result := &Runtime{transport: cfg.Transport, server: server, input: input, output: output}
	switch cfg.Transport {
	case "stdio":
		if input == nil || output == nil {
			return nil, fmt.Errorf("stdio transport requires streams")
		}
	case "http":
		result.httpHandler = result.newHTTPHandler()
	default:
		return nil, fmt.Errorf("unsupported MCP transport %q", cfg.Transport)
	}
	return result, nil
}

func (r *Runtime) Transport() string         { return r.transport }
func (r *Runtime) HTTPHandler() http.Handler { return r.httpHandler }

func (r *Runtime) Serve(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.transport != "stdio" {
		return fmt.Errorf("Serve is only valid for stdio transport")
	}
	scanner := bufio.NewScanner(r.input)
	writer := bufio.NewWriter(r.output)
	defer func() { _ = writer.Flush() }()
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		response := r.server.Handle(ctx, append([]byte(nil), scanner.Bytes()...))
		if _, err := writer.Write(append(response, '\n')); err != nil {
			return err
		}
		if err := writer.Flush(); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (r *Runtime) newHTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var raw json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&raw); err != nil {
			raw = json.RawMessage(`{`)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(r.server.Handle(request.Context(), raw))
	})
}
