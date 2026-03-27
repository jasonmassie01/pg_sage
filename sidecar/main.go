package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
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
	cloudEnvironment   string   // "aurora", "rds", "cloud-sql", "alloydb", "azure", "self-managed"
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
	RateLimit int
	APIKey    string
	TLSCert        string
	TLSKey         string
	PGMaxConns     int32
	PGMinConns     int32
}

func loadConfig() Config {
	cfg := Config{
		DatabaseURL:    envOrDefault("SAGE_DATABASE_URL", "postgres://postgres@localhost:5432/postgres?sslmode=disable"),
		MCPPort:        envOrDefault("SAGE_MCP_PORT", "5433"),
		PrometheusPort: envOrDefault("SAGE_PROMETHEUS_PORT", "9187"),
		RateLimit: envOrDefaultInt("SAGE_RATE_LIMIT", 60),
		APIKey:    os.Getenv("SAGE_API_KEY"),
		TLSCert:        os.Getenv("SAGE_TLS_CERT"),
		TLSKey:         os.Getenv("SAGE_TLS_KEY"),
		PGMaxConns: int32(envOrDefaultInt("SAGE_PG_MAX_CONNS", 10)),
		PGMinConns: int32(envOrDefaultInt("SAGE_PG_MIN_CONNS", 2)),
	}
	return cfg
}

// maxRequestBodySize is the maximum allowed request body (1 MB).
const maxRequestBodySize = 1 << 20

// validTableName matches schema.table or table (alphanumeric + underscores + dots).
var validTableName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*(\.[a-zA-Z_][a-zA-Z0-9_]*)?$`)

// validInteger matches an optional minus and digits.
var validInteger = regexp.MustCompile(`^-?[0-9]+$`)

// validateTableName returns an error if name is not a valid identifier.
func validateTableName(name string) error {
	if !validTableName.MatchString(name) {
		return fmt.Errorf("invalid table name: must be alphanumeric/underscores in table or schema.table format")
	}
	return nil
}

// validateQueryID returns an error if qid is not a valid integer string.
func validateQueryID(qid string) error {
	if !validInteger.MatchString(qid) {
		return fmt.Errorf("invalid queryid: must be an integer")
	}
	return nil
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

	logInfo("startup", "starting — MCP port=%s, Prometheus port=%s", cfg.MCPPort, cfg.PrometheusPort)
	if cfg.APIKey != "" {
		logInfo("startup", "API key authentication enabled")
	} else {
		logWarn("startup", "API key authentication disabled (SAGE_API_KEY not set)")
	}
	if cfg.TLSCert != "" && cfg.TLSKey != "" {
		logInfo("startup", "TLS enabled — cert=%s key=%s", cfg.TLSCert, cfg.TLSKey)
	} else {
		logInfo("startup", "TLS disabled (SAGE_TLS_CERT / SAGE_TLS_KEY not set)")
	}
	logInfo("startup", "PG pool — max_conns=%d, min_conns=%d", cfg.PGMaxConns, cfg.PGMinConns)

	// Connect to PostgreSQL
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		logError("startup", "invalid DATABASE_URL: %v", err)
		os.Exit(1)
	}
	poolCfg.MaxConns = cfg.PGMaxConns
	poolCfg.MinConns = cfg.PGMinConns
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = 30 * time.Second

	pool, err = pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		logError("startup", "cannot create pool: %v", err)
		os.Exit(1)
	}
	defer pool.Close()

	// Verify connectivity
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		logError("startup", "cannot connect to PostgreSQL: %v", err)
		os.Exit(1)
	}
	logInfo("startup", "connected to PostgreSQL")

	// Background pool health check every 30 seconds
	go poolHealthCheck()

	// Detect cloud environment (Aurora, RDS, Cloud SQL, AlloyDB, Azure)
	cloudEnvironment = detectCloudEnvironment()
	logInfo("startup", "cloud environment: %s", cloudEnvironment)

	// Detect whether the pg_sage extension is installed
	extensionAvailable = detectExtension()
	if extensionAvailable {
		logInfo("startup", "mode: EXTENSION — pg_sage schema and functions detected")
	} else {
		logInfo("startup", "mode: SIDECAR-ONLY — pg_sage extension not found, using direct catalog queries")
	}

	// Ensure mcp_log table exists (uses sage schema only when extension is present)
	ensureMCPLogTable()

	// Rate limiter
	rl := NewRateLimiter(cfg.RateLimit)

	// MCP HTTP server
	mcpMux := http.NewServeMux()
	mcpMux.HandleFunc("/sse", handleSSE)
	mcpMux.HandleFunc("/messages", handleMessages)

	handler := securityHeadersMiddleware(
		requestTimeoutMiddleware(
			authMiddleware(cfg.APIKey,
				rateLimitMiddleware(rl, mcpMux))))

	mcpServer := &http.Server{
		Addr:              ":" + cfg.MCPPort,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Prometheus server
	promServer := startPrometheusServer(":" + cfg.PrometheusPort)

	// Start MCP server
	go func() {
		if cfg.TLSCert != "" && cfg.TLSKey != "" {
			logInfo("mcp", "listening on :%s (TLS)", cfg.MCPPort)
			mcpServer.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			if err := mcpServer.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey); err != nil && err != http.ErrServerClosed {
				logError("mcp", "server error: %v", err)
				os.Exit(1)
			}
		} else {
			logInfo("mcp", "listening on :%s (plain HTTP)", cfg.MCPPort)
			if err := mcpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logError("mcp", "server error: %v", err)
				os.Exit(1)
			}
		}
	}()

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	logInfo("shutdown", "shutting down…")

	rl.Stop()

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	if err := mcpServer.Shutdown(shutCtx); err != nil {
		logWarn("shutdown", "MCP server: %v", err)
	}
	if err := promServer.Shutdown(shutCtx); err != nil {
		logWarn("shutdown", "Prometheus server: %v", err)
	}
	logInfo("shutdown", "stopped")
}

// ---------------------------------------------------------------------------
// Pool health check
// ---------------------------------------------------------------------------

func poolHealthCheck() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := pool.Ping(ctx); err != nil {
			logWarn("pool-health", "ping failed: %v", err)
		}
		cancel()
		stat := pool.Stat()
		if stat.TotalConns() == stat.MaxConns() {
			logWarn("pool-health", "pool exhausted — total=%d max=%d idle=%d",
				stat.TotalConns(), stat.MaxConns(), stat.IdleConns())
		}
	}
}

// ---------------------------------------------------------------------------
// Security headers middleware
// ---------------------------------------------------------------------------

func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Request timeout middleware (180s safety net; per-tool timeouts in tools.go)
// ---------------------------------------------------------------------------

func requestTimeoutMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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
		logWarn("startup", "extension detection query failed: %v", err)
		return false
	}
	return exists
}

// ---------------------------------------------------------------------------
// Cloud environment detection
// ---------------------------------------------------------------------------

// detectCloudEnvironment probes the connected PostgreSQL instance to determine
// whether it is running on a known cloud-managed platform.
func detectCloudEnvironment() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Aurora: try SELECT aurora_version()
	var auroraVer string
	if err := pool.QueryRow(ctx, "SELECT aurora_version()").Scan(&auroraVer); err == nil {
		return "aurora"
	}

	// RDS (non-Aurora): check for rds.extensions GUC
	var rdsExt *string
	if err := pool.QueryRow(ctx, "SELECT current_setting('rds.extensions', true)").Scan(&rdsExt); err == nil && rdsExt != nil {
		return "rds"
	}

	// AlloyDB: check for alloydb.iam_authentication or google_columnar_engine.enabled
	var alloyIAM *string
	if err := pool.QueryRow(ctx, "SELECT current_setting('alloydb.iam_authentication', true)").Scan(&alloyIAM); err == nil && alloyIAM != nil {
		return "alloydb"
	}
	var columnarEng *string
	if err := pool.QueryRow(ctx, "SELECT current_setting('google_columnar_engine.enabled', true)").Scan(&columnarEng); err == nil && columnarEng != nil {
		return "alloydb"
	}

	// Cloud SQL: check for cloudsql.iam_authentication
	var cloudSQLIAM *string
	if err := pool.QueryRow(ctx, "SELECT current_setting('cloudsql.iam_authentication', true)").Scan(&cloudSQLIAM); err == nil && cloudSQLIAM != nil {
		return "cloud-sql"
	}

	// Azure: check for azure.extensions
	var azureExt *string
	if err := pool.QueryRow(ctx, "SELECT current_setting('azure.extensions', true)").Scan(&azureExt); err == nil && azureExt != nil {
		return "azure"
	}

	return "self-managed"
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
			logWarn("mcp", "could not create sage.mcp_log: %v", err)
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
			logWarn("mcp", "could not create public.sage_mcp_log: %v", err)
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
		ch:   make(chan []byte, 256),
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

	// Limit request body size to 1 MB
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	// Parse JSON-RPC request
	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON or request too large"}`, http.StatusBadRequest)
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

	// Send response on SSE stream with blocking write + timeout
	data, _ := json.Marshal(resp)
	select {
	case sess.ch <- data:
		// sent
	case <-time.After(10 * time.Second):
		logWarn("mcp", "session %s: write timeout after 10s, dropping response (buf=%d)",
			sessionID, len(sess.ch))
	case <-sess.done:
		logWarn("mcp", "session %s: closed before response sent", sessionID)
	}

	// Also return 202 to the POST caller
	w.WriteHeader(http.StatusAccepted)
}

// ---------------------------------------------------------------------------
// JSON-RPC dispatcher
// ---------------------------------------------------------------------------

func dispatch(ctx context.Context, req JSONRPCRequest) JSONRPCResponse {
	// Check if pool is exhausted before executing DB-backed methods
	if isPoolExhausted() {
		switch req.Method {
		case "resources/read", "tools/call", "prompts/get":
			return rpcErr(req.ID, -32000, "server overloaded: connection pool exhausted, try again later")
		}
	}

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
		if err := validateResourceURI(uri); err != nil {
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
		if err := validateToolArgs(name, args); err != nil {
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

// isPoolExhausted returns true when the PG pool has no idle connections and
// total connections equal max connections.
func isPoolExhausted() bool {
	stat := pool.Stat()
	return stat.IdleConns() == 0 && stat.TotalConns() == stat.MaxConns()
}

// validateResourceURI validates URI parameters embedded in resource URIs.
func validateResourceURI(uri string) error {
	switch {
	case uri == "sage://health", uri == "sage://findings", uri == "sage://slow-queries":
		return nil
	case strings.HasPrefix(uri, "sage://schema/"):
		table := strings.TrimPrefix(uri, "sage://schema/")
		if table == "" {
			return fmt.Errorf("table name required in sage://schema/{table}")
		}
		return validateTableName(table)
	case strings.HasPrefix(uri, "sage://stats/"):
		table := strings.TrimPrefix(uri, "sage://stats/")
		if table == "" {
			return fmt.Errorf("table name required in sage://stats/{table}")
		}
		return validateTableName(table)
	case strings.HasPrefix(uri, "sage://explain/"):
		qid := strings.TrimPrefix(uri, "sage://explain/")
		if qid == "" {
			return fmt.Errorf("queryid required in sage://explain/{queryid}")
		}
		return validateQueryID(qid)
	default:
		return fmt.Errorf("unknown resource URI: %s", uri)
	}
}

// validateToolArgs validates arguments for known tools.
func validateToolArgs(name string, args json.RawMessage) error {
	switch name {
	case "suggest_index":
		var p struct {
			Table string `json:"table"`
		}
		if err := json.Unmarshal(args, &p); err == nil && p.Table != "" {
			return validateTableName(p.Table)
		}
	}
	return nil
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
	if _, err := pool.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %s (client_ip, method, resource_uri, tool_name, tokens_used, duration_ms, status, error_message)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, table),
		ip, req.Method, resourceURI, toolName, 0, int(duration.Milliseconds()), status, errMsg,
	); err != nil {
		logWarn("audit", "failed to log: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Structured logging helpers
// ---------------------------------------------------------------------------

func logInfo(component, msg string, args ...any) {
	logStructured("INFO", component, msg, args...)
}

func logWarn(component, msg string, args ...any) {
	logStructured("WARN", component, msg, args...)
}

func logError(component, msg string, args ...any) {
	logStructured("ERROR", component, msg, args...)
}

func logStructured(level, component, msg string, args ...any) {
	ts := time.Now().UTC().Format(time.RFC3339)
	formatted := fmt.Sprintf(msg, args...)
	fmt.Printf("%s [%s] [%s] %s\n", ts, level, component, formatted)
}
