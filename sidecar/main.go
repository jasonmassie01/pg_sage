package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Global state
// ---------------------------------------------------------------------------

var (
	pool               *pgxpool.Pool
	sessions           sync.Map // sessionID -> *sseSession
	extensionAvailable bool     // true when sage schema + functions are detected
)

type sseSession struct {
	ch   chan []byte // JSON-RPC responses are pushed here
	done chan struct{}
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type Config struct {
	DatabaseURL    string
	MCPPort        string
	PrometheusPort string
	RateLimit      int
	TokenBudget    int
	APIKey         string
}

func loadConfig() Config {
	cfg := Config{
		DatabaseURL:    envOrDefault("SAGE_DATABASE_URL", "postgres://postgres@localhost:5432/postgres?sslmode=disable"),
		MCPPort:        envOrDefault("SAGE_MCP_PORT", "5433"),
		PrometheusPort: envOrDefault("SAGE_PROMETHEUS_PORT", "9187"),
		RateLimit:      envOrDefaultInt("SAGE_RATE_LIMIT", 60),
		TokenBudget:    envOrDefaultInt("SAGE_TOKEN_BUDGET", 10000),
		APIKey:         os.Getenv("SAGE_API_KEY"),
	}
	return cfg
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envOrDefaultInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	cfg := loadConfig()

	log.Printf("[sage-sidecar] starting — MCP port=%s, Prometheus port=%s", cfg.MCPPort, cfg.PrometheusPort)
	if cfg.APIKey != "" {
		log.Println("[sage-sidecar] API key authentication enabled")
	} else {
		log.Println("[sage-sidecar] API key authentication disabled (SAGE_API_KEY not set)")
	}

	// Connect to PostgreSQL
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("invalid DATABASE_URL: %v", err)
	}
	poolCfg.MaxConns = 3

	pool, err = pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		log.Fatalf("cannot create pool: %v", err)
	}
	defer pool.Close()

	// Verify connectivity
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("cannot connect to PostgreSQL: %v", err)
	}
	log.Println("[sage-sidecar] connected to PostgreSQL")

	// Detect whether the pg_sage extension is installed
	extensionAvailable = detectExtension()
	if extensionAvailable {
		log.Println("[sage-sidecar] mode: EXTENSION — pg_sage schema and functions detected")
	} else {
		log.Println("[sage-sidecar] mode: SIDECAR-ONLY — pg_sage extension not found, using direct catalog queries")
	}

	// Ensure mcp_log table exists (uses sage schema only when extension is present)
	ensureMCPLogTable()

	// Rate limiter
	rl := NewRateLimiter(cfg.RateLimit)

	// MCP HTTP server
	mcpMux := http.NewServeMux()
	mcpMux.HandleFunc("/sse", handleSSE)
	mcpMux.HandleFunc("/messages", handleMessages)

	mcpServer := &http.Server{
		Addr:    ":" + cfg.MCPPort,
		Handler: authMiddleware(cfg.APIKey, rateLimitMiddleware(rl, mcpMux)),
	}

	// Prometheus server
	promServer := startPrometheusServer(":" + cfg.PrometheusPort)

	// Start MCP server
	go func() {
		log.Printf("[mcp] listening on :%s", cfg.MCPPort)
		if err := mcpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[mcp] server error: %v", err)
		}
	}()

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("[sage-sidecar] shutting down…")

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	_ = mcpServer.Shutdown(shutCtx)
	_ = promServer.Shutdown(shutCtx)
	log.Println("[sage-sidecar] stopped")
}

// ---------------------------------------------------------------------------
// Extension detection
// ---------------------------------------------------------------------------

// detectExtension checks whether the sage schema and sage.health_json()
// function exist. Returns true when the full pg_sage C extension is installed.
func detectExtension() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var exists bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_namespace WHERE nspname = 'sage'
		) AND EXISTS (
			SELECT 1 FROM pg_proc p
			JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = 'sage' AND p.proname = 'health_json'
		)
	`).Scan(&exists)
	if err != nil {
		log.Printf("[sage-sidecar] extension detection query failed: %v", err)
		return false
	}
	return exists
}

// ---------------------------------------------------------------------------
// Ensure audit log table
// ---------------------------------------------------------------------------

func ensureMCPLogTable() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if extensionAvailable {
		_, err := pool.Exec(ctx, `
			CREATE TABLE IF NOT EXISTS sage.mcp_log (
				id          bigserial PRIMARY KEY,
				ts          timestamptz NOT NULL DEFAULT now(),
				client_ip   text,
				method      text,
				resource_uri text,
				tool_name   text,
				tokens_used int DEFAULT 0,
				duration_ms int DEFAULT 0,
				status      text,
				error_message text
			)
		`)
		if err != nil {
			log.Printf("[mcp] warning: could not create sage.mcp_log: %v", err)
		}
	} else {
		// In sidecar-only mode the sage schema may not exist.
		// Create the log table in the public schema instead.
		_, err := pool.Exec(ctx, `
			CREATE TABLE IF NOT EXISTS public.sage_mcp_log (
				id          bigserial PRIMARY KEY,
				ts          timestamptz NOT NULL DEFAULT now(),
				client_ip   text,
				method      text,
				resource_uri text,
				tool_name   text,
				tokens_used int DEFAULT 0,
				duration_ms int DEFAULT 0,
				status      text,
				error_message text
			)
		`)
		if err != nil {
			log.Printf("[mcp] warning: could not create public.sage_mcp_log: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// SSE handler — GET /sse
// ---------------------------------------------------------------------------

func handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	sessionID := uuid.New().String()
	sess := &sseSession{
		ch:   make(chan []byte, 64),
		done: make(chan struct{}),
	}
	sessions.Store(sessionID, sess)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Send the endpoint event
	fmt.Fprintf(w, "event: endpoint\ndata: /messages?sessionId=%s\n\n", sessionID)
	flusher.Flush()

	// Stream responses until client disconnects
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			close(sess.done)
			sessions.Delete(sessionID)
			return
		case msg := <-sess.ch:
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// Message handler — POST /messages?sessionId=xxx
// ---------------------------------------------------------------------------

func handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		http.Error(w, `{"error":"missing sessionId"}`, http.StatusBadRequest)
		return
	}

	val, ok := sessions.Load(sessionID)
	if !ok {
		http.Error(w, `{"error":"unknown session"}`, http.StatusNotFound)
		return
	}
	sess := val.(*sseSession)

	// Parse JSON-RPC request
	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}

	start := time.Now()
	ip := clientIP(r)

	// Dispatch
	resp := dispatch(r.Context(), req)
	duration := time.Since(start)

	// Audit log (best effort)
	go auditLog(ip, req, duration, resp)

	// For notifications (no ID), just acknowledge with 202
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Send response on SSE stream
	data, _ := json.Marshal(resp)
	select {
	case sess.ch <- data:
	default:
		log.Printf("[mcp] session %s buffer full, dropping response", sessionID)
	}

	// Also return 202 to the POST caller
	w.WriteHeader(http.StatusAccepted)
}

// ---------------------------------------------------------------------------
// JSON-RPC dispatcher
// ---------------------------------------------------------------------------

func dispatch(ctx context.Context, req JSONRPCRequest) JSONRPCResponse {
	switch req.Method {

	case "initialize":
		return rpcOK(req.ID, InitializeResult{
			ProtocolVersion: "2024-11-05",
			Capabilities: ServerCapabilities{
				Resources: &CapabilityObj{ListChanged: false},
				Tools:     &CapabilityObj{ListChanged: false},
				Prompts:   &CapabilityObj{ListChanged: false},
			},
			ServerInfo: ServerInfo{Name: "pg_sage-sidecar", Version: "0.5.0"},
		})

	case "notifications/initialized":
		// Notification — no response needed, but we return empty for logging
		return rpcOK(req.ID, map[string]string{})

	case "ping":
		return rpcOK(req.ID, map[string]string{})

	case "resources/list":
		return rpcOK(req.ID, ResourcesListResult{Resources: resourceCatalogue})

	case "resources/read":
		uri, err := unmarshalResourcesRead(req.Params)
		if err != nil {
			return rpcInvalidParams(req.ID, err.Error())
		}
		result, err := readResource(ctx, uri)
		if err != nil {
			return rpcInternalError(req.ID, err.Error())
		}
		return rpcOK(req.ID, result)

	case "tools/list":
		return rpcOK(req.ID, ToolsListResult{Tools: toolCatalogue})

	case "tools/call":
		name, args, err := unmarshalToolsCall(req.Params)
		if err != nil {
			return rpcInvalidParams(req.ID, err.Error())
		}
		result, err := callTool(ctx, name, args)
		if err != nil {
			return rpcInternalError(req.ID, err.Error())
		}
		return rpcOK(req.ID, result)

	case "prompts/list":
		return rpcOK(req.ID, PromptsListResult{Prompts: promptCatalogue})

	case "prompts/get":
		name, arguments, err := unmarshalPromptsGet(req.Params)
		if err != nil {
			return rpcInvalidParams(req.ID, err.Error())
		}
		result, err := getPrompt(name, arguments)
		if err != nil {
			return rpcInvalidParams(req.ID, err.Error())
		}
		return rpcOK(req.ID, result)

	default:
		return rpcMethodNotFound(req.ID, req.Method)
	}
}

// ---------------------------------------------------------------------------
// Audit logging
// ---------------------------------------------------------------------------

func auditLog(ip string, req JSONRPCRequest, duration time.Duration, resp JSONRPCResponse) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var resourceURI, toolName, status, errMsg *string

	if req.Method == "resources/read" {
		uri, _ := unmarshalResourcesRead(req.Params)
		if uri != "" {
			resourceURI = &uri
		}
	}
	if req.Method == "tools/call" {
		name, _, _ := unmarshalToolsCall(req.Params)
		if name != "" {
			toolName = &name
		}
	}

	st := "ok"
	if resp.Error != nil {
		st = "error"
		msg := resp.Error.Message
		errMsg = &msg
	}
	status = &st

	table := "sage.mcp_log"
	if !extensionAvailable {
		table = "public.sage_mcp_log"
	}
	_, _ = pool.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %s (client_ip, method, resource_uri, tool_name, tokens_used, duration_ms, status, error_message)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, table),
		ip, req.Method, resourceURI, toolName, 0, int(duration.Milliseconds()), status, errMsg,
	)
}
