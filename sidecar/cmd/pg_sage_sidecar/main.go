package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/advisor"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/briefing"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/ha"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/retention"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/startup"
)

// Set by goreleaser ldflags at build time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// Global state.
var (
	pool               *pgxpool.Pool
	sessions           sync.Map
	extensionAvailable bool
	cloudEnvironment   string
	cfg                *config.Config
	coll               *collector.Collector
	anal               *analyzer.Analyzer
	adv                *advisor.Advisor
	llmMgr             *llm.Manager
	exec               *executor.Executor
	haMon              *ha.Monitor
	briefWorker        *briefing.Worker
	llmClient          *llm.Client
	cleaner            *retention.Cleaner
	rampStart          time.Time
	shutdownFlag       bool
	fleetMgr           *fleet.DatabaseManager
	apiServer          *http.Server
)

type sseSession struct {
	ch   chan []byte
	done chan struct{}
}

const maxRequestBodySize = 1 << 20

var validTableName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*(\.[a-zA-Z_][a-zA-Z0-9_]*)?$`)
var validInteger = regexp.MustCompile(`^-?[0-9]+$`)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Printf("pg_sage %s (commit: %s, built: %s)\n", version, commit, date)
		os.Exit(0)
	}

	var err error
	cfg, err = config.Load(os.Args[1:])
	if err != nil {
		logError("startup", "config: %v", err)
		os.Exit(1)
	}

	logInfo("startup", "pg_sage sidecar v%s — mode=%s", version, cfg.Mode)
	logInfo("startup", "MCP=%s Prometheus=%s", cfg.MCP.ListenAddr, cfg.Prometheus.ListenAddr)

	// Build DSN.
	dsn := cfg.Postgres.DSN()
	if dsn == "" {
		dsn = envOrDefault("SAGE_DATABASE_URL",
			"postgres://postgres@localhost:5432/postgres?sslmode=disable")
	}

	// Connect to PostgreSQL.
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		logError("startup", "invalid DSN: %v", err)
		os.Exit(1)
	}
	poolCfg.MaxConns = int32(cfg.Postgres.MaxConnections)
	if poolCfg.MaxConns < 2 {
		poolCfg.MaxConns = 2
	}
	poolCfg.MinConns = 1
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = 30 * time.Second

	pool, err = pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		logError("startup", "pool: %v", err)
		os.Exit(1)
	}
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := pool.Ping(ctx); err != nil {
		logError("startup", "cannot connect: %v", err)
		cancel()
		os.Exit(1)
	}
	cancel()
	logInfo("startup", "connected to PostgreSQL")

	// Background pool health.
	go poolHealthCheck()

	// Cloud environment detection.
	cloudEnvironment = detectCloudEnvironment()
	logInfo("startup", "cloud environment: %s", cloudEnvironment)

	// Extension detection.
	extensionAvailable = detectExtension()
	if extensionAvailable {
		logInfo("startup", "mode: EXTENSION — pg_sage C extension detected")
	} else {
		logInfo("startup", "mode: SIDECAR — no extension, using catalog queries")
	}

	// Standalone mode initialization.
	if cfg.IsStandalone() {
		initStandalone()
	} else {
		ensureMCPLogTable()
	}

	// Fleet manager + REST API.
	initFleetAndAPI()

	// Config hot-reload.
	if cfg.ConfigPath != "" {
		watcher := config.NewWatcher(cfg.ConfigPath, cfg, func(updated *config.Config) {
			cfg = updated
			logInfo("config", "hot-reload applied")
		})
		if err := watcher.Start(); err != nil {
			logWarn("config", "hot-reload disabled: %v", err)
		} else {
			defer watcher.Stop()
		}
	}

	// Rate limiter.
	rl := NewRateLimiter(cfg.RateLimit())

	// MCP HTTP server.
	mcpMux := http.NewServeMux()
	mcpMux.HandleFunc("/sse", handleSSE)
	mcpMux.HandleFunc("/messages", handleMessages)
	mcpMux.HandleFunc("/health", handleHealth)

	handler := securityHeadersMiddleware(
		requestTimeoutMiddleware(
			authMiddleware(cfg.APIKey,
				rateLimitMiddleware(rl, mcpMux))))

	mcpServer := &http.Server{
		Addr:    cfg.MCP.ListenAddr,
		Handler: handler,
	}

	// Prometheus server.
	promServer := startPrometheusServer(cfg.Prometheus.ListenAddr)

	// Start MCP server.
	go func() {
		if cfg.TLSCert != "" && cfg.TLSKey != "" {
			logInfo("mcp", "listening on %s (TLS)", cfg.MCP.ListenAddr)
			mcpServer.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			if err := mcpServer.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey); err != nil && err != http.ErrServerClosed {
				logError("mcp", "server error: %v", err)
				os.Exit(1)
			}
		} else {
			logInfo("mcp", "listening on %s", cfg.MCP.ListenAddr)
			if err := mcpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logError("mcp", "server error: %v", err)
				os.Exit(1)
			}
		}
	}()

	// Graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	logInfo("shutdown", "received %s, shutting down…", sig)
	shutdownFlag = true

	shutCtx, shutCancel := context.WithTimeout(context.Background(),
		time.Duration(cfg.Safety.DDLTimeoutSeconds)*time.Second+10*time.Second)
	defer shutCancel()

	// Release advisory lock if standalone.
	if cfg.IsStandalone() {
		schema.ReleaseAdvisoryLock(shutCtx, pool)
		logInfo("shutdown", "advisory lock released")
	}

	_ = mcpServer.Shutdown(shutCtx)
	_ = promServer.Shutdown(shutCtx)
	if apiServer != nil {
		_ = apiServer.Shutdown(shutCtx)
	}
	logInfo("shutdown", "stopped")
}

func initStandalone() {
	ctx := context.Background()

	// 1. Prerequisite checks.
	logInfo("startup", "running prerequisite checks…")
	checks, err := startup.RunChecks(ctx, pool)
	if err != nil {
		logError("startup", "prerequisite check failed: %v", err)
		os.Exit(1)
	}
	cfg.PGVersionNum = checks.PGVersionNum
	cfg.HasWALColumns = checks.HasWALColumns
	cfg.HasPlanTimeColumns = checks.HasPlanTimeColumns
	logInfo("startup",
		"PG version: %d, WAL columns: %v, plan_time columns: %v, query text: %v",
		checks.PGVersionNum, checks.HasWALColumns,
		checks.HasPlanTimeColumns, checks.QueryTextVisible)
	if !checks.QueryTextVisible {
		logWarn("startup", "query text not visible — GRANT pg_read_all_stats TO %s", cfg.Postgres.User)
	}

	// 2. Schema bootstrap.
	logInfo("startup", "bootstrapping schema…")
	if err := schema.Bootstrap(ctx, pool); err != nil {
		logError("startup", "schema bootstrap: %v", err)
		os.Exit(1)
	}

	// 3. Persist trust ramp start.
	var configRampStart time.Time
	if cfg.Trust.RampStart != "" {
		for _, layout := range []string{
			time.RFC3339, "2006-01-02", "2006-01-02T15:04:05",
		} {
			if parsed, pErr := time.Parse(layout, cfg.Trust.RampStart); pErr == nil {
				configRampStart = parsed
				break
			}
		}
		if configRampStart.IsZero() {
			logWarn("startup", "could not parse trust.ramp_start %q, using now()",
				cfg.Trust.RampStart)
		}
	}
	rampStart, err = schema.PersistTrustRampStart(ctx, pool, configRampStart)
	if err != nil {
		logWarn("startup", "trust ramp start: %v", err)
		rampStart = time.Now()
	}
	logInfo("startup", "trust ramp start: %s (age: %s)",
		rampStart.Format(time.RFC3339), time.Since(rampStart).Round(time.Hour))

	// 4. Verify grants.
	executor.VerifyGrants(ctx, pool, cfg.Postgres.User, logStructuredWrapper)
	if cfg.Trust.Level == "autonomous" && cfg.Trust.Tier3Moderate && cfg.Trust.MaintenanceWindow == "" {
		logWarn("startup", "tier3_moderate enabled without maintenance_window — moderate actions will NOT execute")
	}

	// 5. HA monitor.
	haMon = ha.New(pool, logStructuredWrapper)

	// 6. LLM client.
	llmClient = llm.New(&cfg.LLM, logStructuredWrapper)
	llmMgr = llm.NewManager(llmClient, nil, false)

	// 7. Start collector.
	coll = collector.New(pool, cfg, cfg.PGVersionNum, logStructuredWrapper)
	go coll.Run(context.Background())

	// 8. Start analyzer with v2 index optimizer.
	var opt *optimizer.Optimizer
	if cfg.LLM.Optimizer.Enabled {
		optClient := llmClient
		if cfg.LLM.OptimizerLLM.Enabled {
			optClient = llm.NewOptimizerClient(
				&cfg.LLM, &cfg.LLM.OptimizerLLM, logStructuredWrapper,
			)
			logInfo("startup", "optimizer using dedicated LLM model")
		}
		if optClient.IsEnabled() {
			var fallback *llm.Client
			if cfg.LLM.OptimizerLLM.FallbackToGeneral &&
				optClient != llmClient {
				fallback = llmClient
			}
			opt = optimizer.New(
				optClient, fallback, pool, &cfg.LLM.Optimizer,
				cfg.PGVersionNum, extensionAvailable,
				cfg.LLM.OptimizerLLM.MaxOutputTokens,
				logStructuredWrapper,
			)
			logInfo("startup", "index optimizer v2 enabled (plan_source=%s)",
				cfg.LLM.Optimizer.PlanSource)
		}
	}
	if cfg.Advisor.Enabled && llmClient.IsEnabled() {
		adv = advisor.New(pool, cfg, coll, llmMgr, logStructuredWrapper)
		logInfo("startup", "advisor enabled — interval=%s", cfg.Advisor.Interval())
	}
	var advIface analyzer.ConfigAdvisor
	if adv != nil {
		advIface = adv
	}
	anal = analyzer.New(pool, cfg, coll, opt, advIface, logStructuredWrapper)
	go anal.Run(context.Background())

	// 9. Executor runs after analyzer (called from analyzer loop).
	exec = executor.New(pool, cfg, anal, rampStart, logStructuredWrapper)

	// 10. Briefing worker.
	briefWorker = briefing.New(pool, cfg, llmClient, logStructuredWrapper)

	// 11. Retention cleaner.
	cleaner = retention.New(pool, cfg, logStructuredWrapper)

	// 12. Start orchestrator goroutine — runs executor + retention after each analyzer cycle.
	go standaloneOrchestrator()

	logInfo("startup", "standalone mode initialized — collector=%ds, analyzer=%ds, trust=%s",
		cfg.Collector.IntervalSeconds, cfg.Analyzer.IntervalSeconds, cfg.Trust.Level)
}

func standaloneOrchestrator() {
	// Run executor and retention after each analyzer interval.
	ticker := time.NewTicker(cfg.Analyzer.Interval() + 5*time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if shutdownFlag {
				return
			}
			ctx := context.Background()

			// HA check.
			isReplica := false
			if haMon != nil {
				isReplica = haMon.Check(ctx)
			}

			// Executor.
			if exec != nil {
				exec.RunCycle(ctx, isReplica)
			}

			// Retention.
			if cleaner != nil {
				cleaner.Run(ctx)
			}

			// Update fleet status after each cycle.
			if fleetMgr != nil {
				updateFleetStatus(ctx)
			}
		}
	}
}

func initFleetAndAPI() {
	fleetMgr = fleet.NewManager(cfg)

	dbName := resolveDBName()
	dbCfg := buildDBConfig(dbName)

	inst := &fleet.DatabaseInstance{
		Name:      dbName,
		Config:    dbCfg,
		Pool:      pool,
		Collector: coll,
		Analyzer:  anal,
		Executor:  exec,
		Status: &fleet.InstanceStatus{
			Connected:  true,
			PGVersion:  pgVersionString(cfg.PGVersionNum),
			TrustLevel: cfg.Trust.Level,
			LastSeen:   time.Now(),
		},
	}
	fleetMgr.RegisterInstance(inst)

	startAPIServer()
}

func resolveDBName() string {
	if len(cfg.Databases) > 0 && cfg.Databases[0].Name != "" {
		return cfg.Databases[0].Name
	}
	if cfg.Postgres.Database != "" {
		return cfg.Postgres.Database
	}
	return "default"
}

func buildDBConfig(name string) config.DatabaseConfig {
	if len(cfg.Databases) > 0 {
		return cfg.Databases[0]
	}
	return config.DatabaseConfig{
		Name:     name,
		Host:     cfg.Postgres.Host,
		Port:     cfg.Postgres.Port,
		User:     cfg.Postgres.User,
		Database: cfg.Postgres.Database,
		SSLMode:  cfg.Postgres.SSLMode,
	}
}

func pgVersionString(num int) string {
	if num == 0 {
		return "unknown"
	}
	major := num / 10000
	minor := num % 100
	return fmt.Sprintf("%d.%d", major, minor)
}

func startAPIServer() {
	addr := cfg.API.ListenAddr
	if addr == "" {
		addr = ":8080"
	}

	router := api.NewRouter(fleetMgr, cfg)
	apiServer = &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logInfo("api", "listening on %s", addr)
		if err := apiServer.ListenAndServe(); err != nil &&
			err != http.ErrServerClosed {
			logError("api", "server error: %v", err)
		}
	}()
}

func updateFleetStatus(ctx context.Context) {
	instances := fleetMgr.Instances()
	for _, inst := range instances {
		updateInstanceFindings(ctx, inst)
		break // standalone has one instance
	}
}

func updateInstanceFindings(
	ctx context.Context,
	inst *fleet.DatabaseInstance,
) {
	rows, err := pool.Query(ctx,
		`SELECT severity, count(*)
		   FROM sage.findings
		  WHERE status = 'open'
		  GROUP BY severity`)
	if err != nil {
		logWarn("fleet", "findings query: %v", err)
		return
	}
	defer rows.Close()

	inst.Status.FindingsOpen = 0
	inst.Status.FindingsCritical = 0
	inst.Status.FindingsWarning = 0
	inst.Status.FindingsInfo = 0
	for rows.Next() {
		var sev string
		var cnt int
		if err := rows.Scan(&sev, &cnt); err != nil {
			continue
		}
		inst.Status.FindingsOpen += cnt
		switch sev {
		case "critical":
			inst.Status.FindingsCritical = cnt
		case "warning":
			inst.Status.FindingsWarning = cnt
		case "info":
			inst.Status.FindingsInfo = cnt
		}
	}
	inst.Status.AnalyzerLastRun = time.Now()
	inst.Status.LastSeen = time.Now()
}

// --- Health endpoint ---

func handleHealth(w http.ResponseWriter, r *http.Request) {
	status := map[string]any{
		"version": version,
		"mode":    cfg.Mode,
		"trust":   cfg.Trust.Level,
	}

	if cfg.IsStandalone() {
		status["connection"] = "connected"
		if haMon != nil {
			status["is_replica"] = haMon.IsReplica()
			status["safe_mode"] = haMon.InSafeMode()
		}
		if coll != nil && coll.LatestSnapshot() != nil {
			status["last_collect"] = coll.LatestSnapshot().CollectedAt.Format(time.RFC3339)
		}
		if anal != nil {
			counts := anal.OpenFindingsCount()
			status["findings"] = counts
		}
	} else {
		status["extension"] = extensionAvailable
	}
	status["cloud"] = cloudEnvironment

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// --- SSE/MCP handlers (same as original) ---

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

	fmt.Fprintf(w, "event: endpoint\ndata: /messages?sessionId=%s\n\n", sessionID)
	flusher.Flush()

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

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}

	start := time.Now()
	ip := clientIP(r)

	resp := dispatch(r.Context(), req)
	duration := time.Since(start)

	go auditLog(ip, req, duration, resp)

	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	data, _ := json.Marshal(resp)
	select {
	case sess.ch <- data:
	case <-time.After(10 * time.Second):
		logWarn("mcp", "session %s: write timeout", sessionID)
	case <-sess.done:
	}

	w.WriteHeader(http.StatusAccepted)
}

// --- JSON-RPC types ---

type JSONRPCRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Result  any           `json:"result,omitempty"`
	Error   *JSONRPCError `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type ServerCapabilities struct {
	Resources *CapabilityObj `json:"resources,omitempty"`
	Tools     *CapabilityObj `json:"tools,omitempty"`
	Prompts   *CapabilityObj `json:"prompts,omitempty"`
}

type CapabilityObj struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

// --- Resource types ---

type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

type ResourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
}

type ResourcesListResult struct {
	Resources []Resource `json:"resources"`
}

type ResourcesReadResult struct {
	Contents []ResourceContent `json:"contents"`
}

// --- Tool types ---

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type ToolsListResult struct {
	Tools []Tool `json:"tools"`
}

type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type ToolsCallResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// --- Prompt types ---

type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

type Prompt struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

type PromptsListResult struct {
	Prompts []Prompt `json:"prompts"`
}

type PromptMessage struct {
	Role    string      `json:"role"`
	Content ToolContent `json:"content"`
}

type PromptsGetResult struct {
	Description string          `json:"description,omitempty"`
	Messages    []PromptMessage `json:"messages"`
}

// --- Catalogues ---

var resourceCatalogue = []Resource{
	{URI: "sage://health", Name: "Database Health", Description: "Health snapshot", MimeType: "application/json"},
	{URI: "sage://findings", Name: "Open Findings", Description: "All open findings", MimeType: "application/json"},
	{URI: "sage://slow-queries", Name: "Slow Queries", Description: "Recent slow queries", MimeType: "application/json"},
	{URI: "sage://schema/{table}", Name: "Table Schema", Description: "Column and index info", MimeType: "application/json"},
	{URI: "sage://stats/{table}", Name: "Table Statistics", Description: "Table stats", MimeType: "application/json"},
	{URI: "sage://explain/{queryid}", Name: "Query Plan", Description: "Cached EXPLAIN plan", MimeType: "application/json"},
}

var toolCatalogue []Tool

func buildToolCatalogue() {
	toolCatalogue = []Tool{
		{
			Name: "suggest_index", Description: "Get index suggestions for a table",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"table":{"type":"string"}},"required":["table"]}`),
		},
		{
			Name: "review_migration", Description: "Review DDL for risks",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"ddl":{"type":"string"}},"required":["ddl"]}`),
		},
	}

	if extensionAvailable {
		toolCatalogue = append(toolCatalogue,
			Tool{Name: "diagnose", Description: "Interactive diagnostic question",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string"}},"required":["question"]}`)},
			Tool{Name: "briefing", Description: "Generate health briefing",
				InputSchema: json.RawMessage(`{"type":"object","properties":{},"required":[]}`)},
		)
	}

	// Standalone tools.
	if cfg.IsStandalone() {
		toolCatalogue = append(toolCatalogue,
			Tool{Name: "sage_status", Description: "pg_sage standalone status",
				InputSchema: json.RawMessage(`{"type":"object","properties":{},"required":[]}`)},
			Tool{Name: "sage_emergency_stop", Description: "Emergency stop all autonomous actions",
				InputSchema: json.RawMessage(`{"type":"object","properties":{},"required":[]}`)},
			Tool{Name: "sage_resume", Description: "Resume after emergency stop",
				InputSchema: json.RawMessage(`{"type":"object","properties":{},"required":[]}`)},
			Tool{Name: "sage_briefing", Description: "Generate standalone health briefing",
				InputSchema: json.RawMessage(`{"type":"object","properties":{},"required":[]}`)},
		)
	}
}

var promptCatalogue = []Prompt{
	{Name: "investigate_slow_query", Description: "Investigate a slow query",
		Arguments: []PromptArgument{{Name: "queryid", Description: "Query ID", Required: true}}},
	{Name: "review_schema", Description: "Review table schema design",
		Arguments: []PromptArgument{{Name: "table", Description: "Table name", Required: true}}},
	{Name: "capacity_plan", Description: "Capacity planning analysis"},
}

// --- Dispatcher ---

func dispatch(ctx context.Context, req JSONRPCRequest) JSONRPCResponse {
	if isPoolExhausted() {
		switch req.Method {
		case "resources/read", "tools/call", "prompts/get":
			return rpcErr(req.ID, -32000, "pool exhausted")
		}
	}

	switch req.Method {
	case "initialize":
		buildToolCatalogue()
		result := InitializeResult{
			ProtocolVersion: "2024-11-05",
			Capabilities: ServerCapabilities{
				Resources: &CapabilityObj{},
				Tools:     &CapabilityObj{},
				Prompts:   &CapabilityObj{},
			},
		}
		result.ServerInfo.Name = "pg_sage-sidecar"
		result.ServerInfo.Version = version
		return rpcOK(req.ID, result)

	case "notifications/initialized", "ping":
		return rpcOK(req.ID, map[string]string{})

	case "resources/list":
		return rpcOK(req.ID, ResourcesListResult{Resources: resourceCatalogue})

	case "resources/read":
		var p struct{ URI string `json:"uri"` }
		if err := json.Unmarshal(req.Params, &p); err != nil || p.URI == "" {
			return rpcInvalidParams(req.ID, "uri required")
		}
		if err := validateResourceURI(p.URI); err != nil {
			return rpcInvalidParams(req.ID, err.Error())
		}
		result, err := readResource(ctx, p.URI)
		if err != nil {
			return rpcInternalError(req.ID, err.Error())
		}
		return rpcOK(req.ID, result)

	case "tools/list":
		if len(toolCatalogue) == 0 {
			buildToolCatalogue()
		}
		return rpcOK(req.ID, ToolsListResult{Tools: toolCatalogue})

	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments,omitempty"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			return rpcInvalidParams(req.ID, "tool name required")
		}
		result, err := callTool(ctx, p.Name, p.Arguments)
		if err != nil {
			return rpcInternalError(req.ID, err.Error())
		}
		return rpcOK(req.ID, result)

	case "prompts/list":
		return rpcOK(req.ID, PromptsListResult{Prompts: promptCatalogue})

	case "prompts/get":
		var p struct {
			Name      string            `json:"name"`
			Arguments map[string]string `json:"arguments,omitempty"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			return rpcInvalidParams(req.ID, "prompt name required")
		}
		result, err := getPrompt(p.Name, p.Arguments)
		if err != nil {
			return rpcInvalidParams(req.ID, err.Error())
		}
		return rpcOK(req.ID, result)

	default:
		return rpcMethodNotFound(req.ID, req.Method)
	}
}

// --- Resource handlers ---

func readResource(ctx context.Context, uri string) (ResourcesReadResult, error) {
	var text string
	var err error

	switch {
	case uri == "sage://health":
		text, err = readHealth(ctx)
	case uri == "sage://findings":
		text, err = readFindings(ctx)
	case uri == "sage://slow-queries":
		text, err = readSlowQueries(ctx)
	case strings.HasPrefix(uri, "sage://schema/"):
		text, err = readSchema(ctx, strings.TrimPrefix(uri, "sage://schema/"))
	case strings.HasPrefix(uri, "sage://stats/"):
		text, err = readStats(ctx, strings.TrimPrefix(uri, "sage://stats/"))
	case strings.HasPrefix(uri, "sage://explain/"):
		text, err = readExplain(ctx, strings.TrimPrefix(uri, "sage://explain/"))
	default:
		return ResourcesReadResult{}, fmt.Errorf("unknown resource: %s", uri)
	}

	if err != nil {
		return ResourcesReadResult{}, err
	}
	return ResourcesReadResult{
		Contents: []ResourceContent{{URI: uri, MimeType: "application/json", Text: text}},
	}, nil
}

func readHealth(ctx context.Context) (string, error) {
	if extensionAvailable {
		return queryJSON(ctx, "SELECT sage.health_json()")
	}

	// In standalone mode, include findings summary.
	q := `SELECT json_build_object(
		'mode', 'standalone',
		'version', $1::text,
		'status', 'ok',
		'connections', (SELECT count(*) FROM pg_stat_activity),
		'active_queries', (SELECT count(*) FROM pg_stat_activity WHERE state = 'active'),
		'idle_in_transaction', (SELECT count(*) FROM pg_stat_activity WHERE state = 'idle in transaction'),
		'database_size', pg_size_pretty(pg_database_size(current_database())),
		'database_size_bytes', pg_database_size(current_database()),
		'uptime_seconds', extract(epoch FROM now() - pg_postmaster_start_time())::int,
		'pg_version', version(),
		'max_connections', (SELECT setting::int FROM pg_settings WHERE name = 'max_connections'),
		'cache_hit_ratio', (SELECT round((blks_hit::numeric / nullif(blks_hit + blks_read, 0) * 100), 2) FROM pg_stat_database WHERE datname = current_database()),
		'deadlocks', (SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()),
		'cloud', $2::text
	)::text`
	return queryJSON(ctx, q, version, cloudEnvironment)
}

func readFindings(ctx context.Context) (string, error) {
	if extensionAvailable {
		var result string
		err := pool.QueryRow(ctx, "SELECT sage.findings_json('open')").Scan(&result)
		if err == nil {
			return annotateAlterSystem(result), nil
		}
	}

	// Standalone: read from sage.findings table.
	if cfg.IsStandalone() {
		var result string
		err := pool.QueryRow(ctx, `
			SELECT coalesce(
				(SELECT json_agg(row_to_json(f) ORDER BY
					CASE f.severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,
					f.last_seen DESC
				) FROM sage.findings f WHERE f.status = 'open'),
				'[]'::json
			)::text
		`).Scan(&result)
		if err != nil {
			return "[]", nil
		}
		return annotateAlterSystem(result), nil
	}

	return `{"note":"findings require pg_sage extension or standalone mode","findings":[]}`, nil
}

func readSlowQueries(ctx context.Context) (string, error) {
	if extensionAvailable {
		var result string
		if err := pool.QueryRow(ctx, "SELECT sage.slow_queries_json()").Scan(&result); err == nil {
			return result, nil
		}
	}
	return queryJSON(ctx, `SELECT coalesce(
		(SELECT json_agg(row_to_json(s))
		 FROM (
			SELECT queryid, query, calls, mean_exec_time,
			       total_exec_time, rows, shared_blks_hit, shared_blks_read
			FROM pg_stat_statements
			ORDER BY mean_exec_time DESC LIMIT 20
		 ) s), '[]'::json)::text`)
}

func readSchema(ctx context.Context, table string) (string, error) {
	t := sanitize(table)
	return queryJSON(ctx, `SELECT json_build_object(
		'table', $1::text,
		'columns', (
			SELECT coalesce(json_agg(json_build_object(
				'name', column_name, 'type', data_type, 'nullable', is_nullable,
				'column_default', column_default
			) ORDER BY ordinal_position), '[]'::json)
			FROM information_schema.columns
			WHERE table_schema || '.' || table_name = $1::text OR table_name = $1::text
		),
		'indexes', (
			SELECT coalesce(json_agg(json_build_object(
				'name', indexname, 'def', indexdef
			)), '[]'::json) FROM pg_indexes
			WHERE schemaname || '.' || tablename = $1::text OR tablename = $1::text
		),
		'constraints', (
			SELECT coalesce(json_agg(json_build_object(
				'name', con.conname, 'type', con.contype,
				'definition', pg_get_constraintdef(con.oid)
			)), '[]'::json)
			FROM pg_constraint con
			JOIN pg_class rel ON rel.oid = con.conrelid
			JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
			WHERE nsp.nspname || '.' || rel.relname = $1::text OR rel.relname = $1::text
		)
	)::text`, t)
}

func readStats(ctx context.Context, table string) (string, error) {
	t := sanitize(table)
	return queryJSON(ctx, `SELECT row_to_json(s)::text FROM (
		SELECT relname, schemaname, seq_scan, seq_tup_read, idx_scan, idx_tup_fetch,
		       n_tup_ins, n_tup_upd, n_tup_del, n_live_tup, n_dead_tup,
		       last_vacuum, last_autovacuum, last_analyze, last_autoanalyze,
		       pg_size_pretty(pg_total_relation_size(schemaname || '.' || relname)) AS total_size,
		       pg_size_pretty(pg_relation_size(schemaname || '.' || relname)) AS table_size,
		       pg_size_pretty(pg_indexes_size(schemaname || '.' || relname)) AS index_size
		FROM pg_stat_user_tables
		WHERE schemaname || '.' || relname = $1::text OR relname = $1::text
		LIMIT 1
	) s`, t)
}

func readExplain(ctx context.Context, queryid string) (string, error) {
	qid := sanitize(queryid)
	if extensionAvailable {
		var result string
		if err := pool.QueryRow(ctx,
			"SELECT coalesce(sage.explain_json($1), '{\"error\":\"no cached plan\"}')",
			qid).Scan(&result); err == nil {
			return result, nil
		}
	}
	if cfg.IsStandalone() {
		var result string
		err := pool.QueryRow(ctx, `
			SELECT coalesce(
				(SELECT plan_json::text FROM sage.explain_cache
				 WHERE queryid = $1::bigint ORDER BY captured_at DESC LIMIT 1),
				'{"error":"no cached plan"}'
			)`, qid).Scan(&result)
		if err == nil {
			return result, nil
		}
	}
	return `{"note":"explain cache requires extension or standalone mode"}`, nil
}

// --- Tool handlers ---

func callTool(ctx context.Context, name string, args json.RawMessage) (ToolsCallResult, error) {
	timeout := 120 * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch name {
	case "diagnose":
		if !extensionAvailable {
			return toolErr("diagnose requires pg_sage extension"), nil
		}
		var p struct{ Question string `json:"question"` }
		json.Unmarshal(args, &p)
		var result string
		err := pool.QueryRow(ctx, "SELECT sage.diagnose($1)", p.Question).Scan(&result)
		if err != nil {
			return toolErr(err.Error()), nil
		}
		return toolOK(result), nil

	case "briefing":
		if !extensionAvailable {
			return toolErr("briefing requires pg_sage extension"), nil
		}
		var result string
		err := pool.QueryRow(ctx, "SELECT sage.briefing()").Scan(&result)
		if err != nil {
			return toolErr(err.Error()), nil
		}
		return toolOK(result), nil

	case "suggest_index":
		var p struct{ Table string `json:"table"` }
		json.Unmarshal(args, &p)
		t := sanitize(p.Table)
		var result string
		err := pool.QueryRow(ctx, `SELECT json_build_object(
			'table', $1::text,
			'analysis', json_build_object(
				'seq_scan', s.seq_scan, 'idx_scan', coalesce(s.idx_scan,0),
				'n_live_tup', s.n_live_tup,
				'seq_scan_ratio', CASE WHEN (s.seq_scan + coalesce(s.idx_scan,0)) > 0
					THEN round(s.seq_scan::numeric / (s.seq_scan + coalesce(s.idx_scan,0)), 4) ELSE 0 END
			),
			'existing_indexes', (
				SELECT coalesce(json_agg(json_build_object('name', indexname, 'def', indexdef)), '[]'::json)
				FROM pg_indexes WHERE schemaname || '.' || tablename = $1::text OR tablename = $1::text
			)
		)::text FROM pg_stat_user_tables s
		WHERE s.schemaname || '.' || s.relname = $1::text OR s.relname = $1::text LIMIT 1`, t).Scan(&result)
		if err != nil {
			return toolErr(fmt.Sprintf("table %s: %v", t, err)), nil
		}
		return toolOK(result), nil

	case "review_migration":
		var p struct{ DDL string `json:"ddl"` }
		json.Unmarshal(args, &p)
		review := reviewDDL(p.DDL)
		out, _ := json.Marshal(review)
		return toolOK(string(out)), nil

	case "sage_status":
		return toolOK(sageStatus()), nil

	case "sage_emergency_stop":
		err := executor.SetEmergencyStop(ctx, pool, true)
		if err != nil {
			return toolErr(err.Error()), nil
		}
		return toolOK("Emergency stop activated. All autonomous actions suspended."), nil

	case "sage_resume":
		err := executor.SetEmergencyStop(ctx, pool, false)
		if err != nil {
			return toolErr(err.Error()), nil
		}
		return toolOK("Resumed. Autonomous actions re-enabled."), nil

	case "sage_briefing":
		if briefWorker == nil {
			return toolErr("briefing not available"), nil
		}
		text, err := briefWorker.Generate(ctx)
		if err != nil {
			return toolErr(err.Error()), nil
		}
		return toolOK(text), nil

	default:
		return ToolsCallResult{
			Content: []ToolContent{{Type: "text", Text: "unknown tool: " + name}},
			IsError: true,
		}, nil
	}
}

func sageStatus() string {
	status := map[string]any{
		"version":    version,
		"mode":       cfg.Mode,
		"trust":      cfg.Trust.Level,
		"cloud":      cloudEnvironment,
		"ramp_start": rampStart.Format(time.RFC3339),
		"ramp_age":   time.Since(rampStart).String(),
	}
	if anal != nil {
		status["findings"] = anal.OpenFindingsCount()
	}
	if haMon != nil {
		status["is_replica"] = haMon.IsReplica()
		status["safe_mode"] = haMon.InSafeMode()
	}
	out, _ := json.MarshalIndent(status, "", "  ")
	return string(out)
}

func reviewDDL(ddl string) map[string]any {
	upper := strings.ToUpper(ddl)
	var warnings []string

	checks := []struct{ pattern, msg string }{
		{"DROP TABLE", "DROP TABLE: permanently deletes table and data"},
		{"DROP COLUMN", "DROP COLUMN: irreversible, may break applications"},
		{"TRUNCATE", "TRUNCATE: removes all rows, ACCESS EXCLUSIVE lock"},
		{"LOCK TABLE", "Explicit table lock detected"},
		{"RENAME", "RENAME: may break application queries"},
	}
	for _, c := range checks {
		if strings.Contains(upper, c.pattern) {
			warnings = append(warnings, c.msg)
		}
	}
	if strings.Contains(upper, "CREATE INDEX") && !strings.Contains(upper, "CONCURRENTLY") {
		warnings = append(warnings, "CREATE INDEX without CONCURRENTLY: blocks writes")
	}
	if strings.Contains(upper, "NOT NULL") && !strings.Contains(upper, "CREATE TABLE") {
		warnings = append(warnings, "Adding NOT NULL: requires full table scan on large tables")
	}
	if len(warnings) == 0 {
		warnings = []string{"No obvious risks. Standard review recommended."}
	}
	return map[string]any{"ddl": ddl, "warnings": warnings}
}

// --- Prompt handlers ---

func getPrompt(name string, args map[string]string) (PromptsGetResult, error) {
	switch name {
	case "investigate_slow_query":
		qid := args["queryid"]
		if qid == "" {
			return PromptsGetResult{}, fmt.Errorf("queryid required")
		}
		return PromptsGetResult{
			Description: "Investigate slow query " + qid,
			Messages: []PromptMessage{{
				Role: "user",
				Content: ToolContent{Type: "text",
					Text: fmt.Sprintf("Investigate why query %s is slow.", sanitizePromptArg(qid))},
			}},
		}, nil
	case "review_schema":
		table := args["table"]
		if table == "" {
			return PromptsGetResult{}, fmt.Errorf("table required")
		}
		return PromptsGetResult{
			Description: "Review schema for " + table,
			Messages: []PromptMessage{{
				Role: "user",
				Content: ToolContent{Type: "text",
					Text: fmt.Sprintf("Review schema design of table %s.", sanitizePromptArg(table))},
			}},
		}, nil
	case "capacity_plan":
		return PromptsGetResult{
			Description: "Capacity planning analysis",
			Messages: []PromptMessage{{
				Role:    "user",
				Content: ToolContent{Type: "text", Text: "Analyze database capacity and growth trends."},
			}},
		}, nil
	default:
		return PromptsGetResult{}, fmt.Errorf("unknown prompt: %s", name)
	}
}

func sanitizePromptArg(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 32 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// --- Prometheus ---

func startPrometheusServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", handleMetrics)
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		logInfo("prometheus", "listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logError("prometheus", "server error: %v", err)
		}
	}()
	return srv
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var b strings.Builder

	// Info metric.
	b.WriteString("# HELP pg_sage_info pg_sage version\n# TYPE pg_sage_info gauge\n")
	if extensionAvailable {
		var ver string
		if err := pool.QueryRow(ctx, "SELECT sage.status()->>'version'").Scan(&ver); err != nil {
			ver = "unknown"
		}
		fmt.Fprintf(&b, "pg_sage_info{version=%q,mode=\"extension\"} 1\n\n", ver)
	} else {
		fmt.Fprintf(&b, "pg_sage_info{version=%q,mode=%q} 1\n\n", version, cfg.Mode)
	}

	// Mode metric.
	b.WriteString("# HELP pg_sage_mode Operating mode (0=extension, 1=standalone)\n# TYPE pg_sage_mode gauge\n")
	modeVal := 0
	if cfg.IsStandalone() {
		modeVal = 1
	}
	fmt.Fprintf(&b, "pg_sage_mode %d\n\n", modeVal)

	// Connection metric.
	b.WriteString("# HELP pg_sage_connection_up PostgreSQL connection status\n# TYPE pg_sage_connection_up gauge\n")
	connUp := 0
	if err := pool.Ping(ctx); err == nil {
		connUp = 1
	}
	fmt.Fprintf(&b, "pg_sage_connection_up %d\n\n", connUp)

	if extensionAvailable {
		writeExtensionMetrics(&b, ctx)
	}

	// Standalone metrics.
	if cfg.IsStandalone() {
		writeStandaloneMetrics(&b, ctx)
	}

	// Database metrics (always).
	writeDatabaseMetrics(&b, ctx)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprint(w, b.String())
}

func writeExtensionMetrics(b *strings.Builder, ctx context.Context) {
	// Findings.
	b.WriteString("# HELP pg_sage_findings_total Open findings by severity\n# TYPE pg_sage_findings_total gauge\n")
	rows, err := pool.Query(ctx, `SELECT severity, count(*) FROM sage.findings WHERE status = 'open' GROUP BY severity`)
	if err == nil {
		defer rows.Close()
		found := map[string]int64{}
		for rows.Next() {
			var sev string
			var cnt int64
			if rows.Scan(&sev, &cnt) == nil {
				found[sev] = cnt
			}
		}
		for _, sev := range []string{"critical", "warning", "info"} {
			fmt.Fprintf(b, "pg_sage_findings_total{severity=%q} %d\n", sev, found[sev])
		}
		b.WriteString("\n")
	}

	// Circuit breaker.
	b.WriteString("# HELP pg_sage_circuit_breaker_state Circuit breaker (0=closed, 1=open)\n# TYPE pg_sage_circuit_breaker_state gauge\n")
	var statusJSON string
	if err := pool.QueryRow(ctx, "SELECT sage.status()::text").Scan(&statusJSON); err == nil {
		var status map[string]any
		if json.Unmarshal([]byte(statusJSON), &status) == nil {
			dbState, llmState := 0, 0
			if v, ok := status["circuit_state"].(string); ok && v != "closed" {
				dbState = 1
			}
			if v, ok := status["llm_circuit_state"].(string); ok && v != "closed" {
				llmState = 1
			}
			fmt.Fprintf(b, "pg_sage_circuit_breaker_state{breaker=\"db\"} %d\n", dbState)
			fmt.Fprintf(b, "pg_sage_circuit_breaker_state{breaker=\"llm\"} %d\n", llmState)
		}
	}
	b.WriteString("\n")
}

func writeStandaloneMetrics(b *strings.Builder, ctx context.Context) {
	// Findings from sage.findings table.
	b.WriteString("# HELP pg_sage_findings_total Open findings by severity\n# TYPE pg_sage_findings_total gauge\n")
	if anal != nil {
		counts := anal.OpenFindingsCount()
		for _, sev := range []string{"critical", "warning", "info"} {
			fmt.Fprintf(b, "pg_sage_findings_total{severity=%q} %d\n", sev, counts[sev])
		}
	}
	b.WriteString("\n")

	// Collector metrics.
	if coll != nil {
		snap := coll.LatestSnapshot()
		if snap != nil {
			b.WriteString("# HELP pg_sage_collector_last_run_timestamp Last collector run\n# TYPE pg_sage_collector_last_run_timestamp gauge\n")
			fmt.Fprintf(b, "pg_sage_collector_last_run_timestamp %d\n\n", snap.CollectedAt.Unix())
		}
	}

	// LLM metrics.
	if llmClient != nil {
		b.WriteString("# HELP pg_sage_llm_enabled LLM integration enabled\n# TYPE pg_sage_llm_enabled gauge\n")
		enabled := 0
		if llmClient.IsEnabled() {
			enabled = 1
		}
		fmt.Fprintf(b, "pg_sage_llm_enabled %d\n\n", enabled)

		b.WriteString("# HELP pg_sage_llm_circuit_open LLM circuit breaker (0=closed, 1=open)\n# TYPE pg_sage_llm_circuit_open gauge\n")
		circuitVal := 0
		if llmClient.IsCircuitOpen() {
			circuitVal = 1
		}
		fmt.Fprintf(b, "pg_sage_llm_circuit_open %d\n\n", circuitVal)

		b.WriteString("# HELP pg_sage_llm_tokens_used_today Tokens consumed today\n# TYPE pg_sage_llm_tokens_used_today gauge\n")
		fmt.Fprintf(b, "pg_sage_llm_tokens_used_today %d\n\n", llmClient.TokensUsedToday())

		b.WriteString("# HELP pg_sage_llm_tokens_budget_daily Daily token budget\n# TYPE pg_sage_llm_tokens_budget_daily gauge\n")
		fmt.Fprintf(b, "pg_sage_llm_tokens_budget_daily %d\n\n", cfg.LLM.TokenBudgetDaily)
	}

	// Optimizer metrics from sage.findings.
	writeOptimizerMetrics(b, ctx)
}

func writeOptimizerMetrics(b *strings.Builder, ctx context.Context) {
	b.WriteString("# HELP pg_sage_optimizer_recommendations_total Index recommendations by category\n")
	b.WriteString("# TYPE pg_sage_optimizer_recommendations_total gauge\n")

	rows, err := pool.Query(ctx,
		`SELECT category, count(*)
		 FROM sage.findings
		 WHERE status = 'open'
		   AND category IN ('missing_index','covering_index','partial_index','composite_index')
		 GROUP BY category`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var cat string
			var cnt int64
			if rows.Scan(&cat, &cnt) == nil {
				fmt.Fprintf(b, "pg_sage_optimizer_recommendations_total{category=%q} %d\n", cat, cnt)
			}
		}
	}
	b.WriteString("\n")

	b.WriteString("# HELP pg_sage_optimizer_enabled Optimizer v2 enabled\n")
	b.WriteString("# TYPE pg_sage_optimizer_enabled gauge\n")
	optEnabled := 0
	if cfg.LLM.Optimizer.Enabled {
		optEnabled = 1
	}
	fmt.Fprintf(b, "pg_sage_optimizer_enabled %d\n\n", optEnabled)
}

func writeDatabaseMetrics(b *strings.Builder, ctx context.Context) {
	// Connections.
	b.WriteString("# HELP pg_sage_connections_total Connections by state\n# TYPE pg_sage_connections_total gauge\n")
	rows, err := pool.Query(ctx, `SELECT coalesce(state, 'unknown'), count(*) FROM pg_stat_activity GROUP BY state`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var state string
			var cnt int64
			if rows.Scan(&state, &cnt) == nil {
				fmt.Fprintf(b, "pg_sage_connections_total{state=%q} %d\n", state, cnt)
			}
		}
		b.WriteString("\n")
	}

	// Database size.
	var dbSize int64
	if pool.QueryRow(ctx, "SELECT pg_database_size(current_database())").Scan(&dbSize) == nil {
		b.WriteString("# HELP pg_sage_database_size_bytes Database size\n# TYPE pg_sage_database_size_bytes gauge\n")
		fmt.Fprintf(b, "pg_sage_database_size_bytes %d\n\n", dbSize)
	}

	// Cache hit ratio.
	var hit, read int64
	if pool.QueryRow(ctx, `SELECT blks_hit, blks_read FROM pg_stat_database WHERE datname = current_database()`).Scan(&hit, &read) == nil && (hit+read) > 0 {
		ratio := float64(hit) / float64(hit+read)
		b.WriteString("# HELP pg_sage_cache_hit_ratio Buffer cache hit ratio\n# TYPE pg_sage_cache_hit_ratio gauge\n")
		fmt.Fprintf(b, "pg_sage_cache_hit_ratio %g\n\n", ratio)
	}
}

// --- Middleware ---

func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func requestTimeoutMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func authMiddleware(apiKey string, next http.Handler) http.Handler {
	if apiKey == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if header == "" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"missing Authorization"}`))
			return
		}
		token := strings.TrimPrefix(header, "Bearer ")
		if token == header || token != apiKey {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid API key"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- Rate limiter ---

type RateLimiter struct {
	mu       sync.Mutex
	windows  map[string][]time.Time
	limit    int
	interval time.Duration
}

func NewRateLimiter(maxPerMinute int) *RateLimiter {
	rl := &RateLimiter{
		windows:  make(map[string][]time.Time),
		limit:    maxPerMinute,
		interval: time.Minute,
	}
	go rl.cleanup()
	return rl
}

func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-rl.interval)
	ts := rl.windows[ip]
	start := 0
	for start < len(ts) && ts[start].Before(cutoff) {
		start++
	}
	ts = ts[start:]
	if len(ts) >= rl.limit {
		rl.windows[ip] = ts
		return false
	}
	rl.windows[ip] = append(ts, now)
	return true
}

func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(2 * time.Minute)
	for range ticker.C {
		rl.mu.Lock()
		cutoff := time.Now().Add(-rl.interval)
		for ip, ts := range rl.windows {
			start := 0
			for start < len(ts) && ts[start].Before(cutoff) {
				start++
			}
			if start >= len(ts) {
				delete(rl.windows, ip)
			} else {
				rl.windows[ip] = ts[start:]
			}
		}
		rl.mu.Unlock()
	}
}

func rateLimitMiddleware(rl *RateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.Allow(clientIP(r)) {
			http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return xff[:i]
		}
		return xff
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- Helpers ---

func queryJSON(ctx context.Context, q string, args ...any) (string, error) {
	var result string
	err := pool.QueryRow(ctx, q, args...).Scan(&result)
	return result, err
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func validateResourceURI(uri string) error {
	switch {
	case uri == "sage://health", uri == "sage://findings", uri == "sage://slow-queries":
		return nil
	case strings.HasPrefix(uri, "sage://schema/"):
		t := strings.TrimPrefix(uri, "sage://schema/")
		if t == "" || !validTableName.MatchString(t) {
			return fmt.Errorf("invalid table name")
		}
	case strings.HasPrefix(uri, "sage://stats/"):
		t := strings.TrimPrefix(uri, "sage://stats/")
		if t == "" || !validTableName.MatchString(t) {
			return fmt.Errorf("invalid table name")
		}
	case strings.HasPrefix(uri, "sage://explain/"):
		q := strings.TrimPrefix(uri, "sage://explain/")
		if q == "" || !validInteger.MatchString(q) {
			return fmt.Errorf("invalid queryid")
		}
	default:
		return fmt.Errorf("unknown resource: %s", uri)
	}
	return nil
}

func annotateAlterSystem(text string) string {
	if (cloudEnvironment == "aurora" || cloudEnvironment == "rds") &&
		strings.Contains(strings.ToUpper(text), "ALTER SYSTEM") {
		return strings.ReplaceAll(text, "ALTER SYSTEM",
			"ALTER SYSTEM (Note: On RDS/Aurora use parameter groups)")
	}
	return text
}

func isPoolExhausted() bool {
	stat := pool.Stat()
	return stat.IdleConns() == 0 && stat.TotalConns() == stat.MaxConns()
}

func toolOK(text string) ToolsCallResult {
	return ToolsCallResult{Content: []ToolContent{{Type: "text", Text: text}}}
}

func toolErr(text string) ToolsCallResult {
	return ToolsCallResult{Content: []ToolContent{{Type: "text", Text: text}}, IsError: true}
}

func rpcOK(id *json.RawMessage, result any) JSONRPCResponse {
	return JSONRPCResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func rpcErr(id *json.RawMessage, code int, msg string) JSONRPCResponse {
	return JSONRPCResponse{JSONRPC: "2.0", ID: id, Error: &JSONRPCError{Code: code, Message: msg}}
}

func rpcMethodNotFound(id *json.RawMessage, method string) JSONRPCResponse {
	return rpcErr(id, -32601, "method not found: "+method)
}

func rpcInvalidParams(id *json.RawMessage, msg string) JSONRPCResponse {
	return rpcErr(id, -32602, msg)
}

func rpcInternalError(id *json.RawMessage, msg string) JSONRPCResponse {
	return rpcErr(id, -32603, msg)
}

// --- Audit log ---

func auditLog(ip string, req JSONRPCRequest, duration time.Duration, resp JSONRPCResponse) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var resourceURI, toolName *string
	if req.Method == "resources/read" {
		var p struct{ URI string `json:"uri"` }
		if json.Unmarshal(req.Params, &p) == nil && p.URI != "" {
			resourceURI = &p.URI
		}
	}
	if req.Method == "tools/call" {
		var p struct{ Name string `json:"name"` }
		if json.Unmarshal(req.Params, &p) == nil && p.Name != "" {
			toolName = &p.Name
		}
	}

	st := "ok"
	var errMsg *string
	if resp.Error != nil {
		st = "error"
		errMsg = &resp.Error.Message
	}

	table := "sage.mcp_log"
	if !extensionAvailable && !cfg.IsStandalone() {
		table = "public.sage_mcp_log"
	}
	_, _ = pool.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %s (client_ip, method, resource_uri, tool_name, duration_ms, status, error_message)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, table),
		ip, req.Method, resourceURI, toolName, int(duration.Milliseconds()), st, errMsg)
}

// --- Detection ---

func detectExtension() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var exists bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'sage')
		AND EXISTS (
			SELECT 1 FROM pg_proc p
			JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = 'sage' AND p.proname = 'health_json'
		)
	`).Scan(&exists)
	if err != nil {
		return false
	}
	return exists
}

func detectCloudEnvironment() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var s string
	if pool.QueryRow(ctx, "SELECT aurora_version()").Scan(&s) == nil {
		return "aurora"
	}
	var ps *string
	if pool.QueryRow(ctx, "SELECT current_setting('rds.extensions', true)").Scan(&ps) == nil && ps != nil {
		return "rds"
	}
	if pool.QueryRow(ctx, "SELECT current_setting('alloydb.iam_authentication', true)").Scan(&ps) == nil && ps != nil {
		return "alloydb"
	}
	if pool.QueryRow(ctx, "SELECT current_setting('cloudsql.iam_authentication', true)").Scan(&ps) == nil && ps != nil {
		return "cloud-sql"
	}
	if pool.QueryRow(ctx, "SELECT current_setting('azure.extensions', true)").Scan(&ps) == nil && ps != nil {
		return "azure"
	}
	return "self-managed"
}

func ensureMCPLogTable() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	table := `CREATE TABLE IF NOT EXISTS %s (
		id bigserial PRIMARY KEY, ts timestamptz NOT NULL DEFAULT now(),
		client_ip text, method text, resource_uri text, tool_name text,
		tokens_used int DEFAULT 0, duration_ms int DEFAULT 0,
		status text, error_message text)`
	if extensionAvailable {
		pool.Exec(ctx, fmt.Sprintf(table, "sage.mcp_log"))
	} else {
		pool.Exec(ctx, fmt.Sprintf(table, "public.sage_mcp_log"))
	}
}

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
		if stat.TotalConns() == stat.MaxConns() && stat.IdleConns() == 0 {
			logWarn("pool-health", "exhausted — total=%d max=%d", stat.TotalConns(), stat.MaxConns())
		}
	}
}

// --- Logging ---

func logInfo(component, msg string, args ...any)  { logStructured("INFO", component, msg, args...) }
func logWarn(component, msg string, args ...any)  { logStructured("WARN", component, msg, args...) }
func logError(component, msg string, args ...any) { logStructured("ERROR", component, msg, args...) }

func logStructured(level, component, msg string, args ...any) {
	ts := time.Now().UTC().Format(time.RFC3339)
	fmt.Printf("%s [%s] [%s] %s\n", ts, level, component, fmt.Sprintf(msg, args...))
}

func logStructuredWrapper(component, msg string, args ...any) {
	logStructured("INFO", component, msg, args...)
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
