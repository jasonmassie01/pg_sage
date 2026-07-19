package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// DatabaseDeps holds dependencies for managed database handlers.
type DatabaseDeps struct {
	Store       *store.DatabaseStore
	Fleet       *fleet.DatabaseManager
	ApplyCreate func(
		context.Context, store.DatabaseInput, int,
	) (*store.DatabaseRecord, error)
	OnCreate    func(context.Context, store.DatabaseRecord) error
	ApplyUpdate func(
		context.Context, int, store.DatabaseRecord, store.DatabaseInput,
	) (*store.DatabaseRecord, error)
	OnUpdate func(
		context.Context, store.DatabaseRecord, store.DatabaseRecord,
	) error
	ApplyDelete func(context.Context, int, store.DatabaseRecord) error
}

// registerDatabaseRoutes registers /api/v1/databases/managed
// endpoints. All require admin role.
func registerDatabaseRoutes(
	mux *http.ServeMux, deps *DatabaseDeps,
) {
	adminOnly := RequireRole("admin")

	list := adminOnly(http.HandlerFunc(
		listManagedDBHandler(deps)))
	mux.Handle("GET /api/v1/databases/managed", list)

	create := adminOnly(http.HandlerFunc(
		createManagedDBHandler(deps)))
	mux.Handle("POST /api/v1/databases/managed", create)

	importH := adminOnly(http.HandlerFunc(
		importCSVHandler(deps)))
	mux.Handle(
		"POST /api/v1/databases/managed/import", importH)

	getH := adminOnly(http.HandlerFunc(
		getManagedDBHandler(deps)))
	mux.Handle("GET /api/v1/databases/managed/{id}", getH)

	updateH := adminOnly(http.HandlerFunc(
		updateManagedDBHandler(deps)))
	mux.Handle(
		"PUT /api/v1/databases/managed/{id}", updateH)

	deleteH := adminOnly(http.HandlerFunc(
		deleteManagedDBHandler(deps)))
	mux.Handle(
		"DELETE /api/v1/databases/managed/{id}", deleteH)

	testH := adminOnly(http.HandlerFunc(
		testManagedDBHandler(deps)))
	mux.Handle(
		"POST /api/v1/databases/managed/{id}/test", testH)

	testPreview := adminOnly(http.HandlerFunc(
		testConnectionPreviewHandler()))
	mux.Handle(
		"POST /api/v1/databases/managed/test-connection",
		testPreview)
}

func listManagedDBHandler(
	deps *DatabaseDeps,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		records, err := deps.Store.List(r.Context())
		if err != nil {
			jsonError(w, "failed to list databases",
				http.StatusInternalServerError)
			return
		}
		out := make([]map[string]any, 0, len(records))
		for _, rec := range records {
			out = append(out, dbRecordToMap(rec))
		}
		jsonResponse(w, map[string]any{"databases": out})
	}
}

func getManagedDBHandler(
	deps *DatabaseDeps,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			jsonError(w, "invalid database ID",
				http.StatusBadRequest)
			return
		}
		rec, err := deps.Store.Get(r.Context(), id)
		if err != nil {
			jsonError(w, "database not found",
				http.StatusNotFound)
			return
		}
		jsonResponse(w, dbRecordToMap(*rec))
	}
}

func createManagedDBHandler(
	deps *DatabaseDeps,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req dbCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body",
				http.StatusBadRequest)
			return
		}
		input := req.toInput()
		user := UserFromContext(r.Context())
		createdBy := 0
		if user != nil {
			createdBy = user.ID
		}
		if deps.ApplyCreate != nil {
			rec, applyErr := deps.ApplyCreate(
				r.Context(), input, createdBy,
			)
			if applyErr != nil {
				if errors.Is(applyErr, store.ErrValidation) {
					jsonError(w, applyErr.Error(), http.StatusBadRequest)
					return
				}
				internalError(w, r, "create and activate database", applyErr)
				return
			}
			w.WriteHeader(http.StatusCreated)
			jsonResponse(w, dbRecordToMap(*rec))
			return
		}
		id, err := deps.Store.Create(
			r.Context(), input, createdBy)
		if err != nil {
			if errors.Is(err, store.ErrValidation) {
				jsonError(w, err.Error(),
					http.StatusBadRequest)
				return
			}
			internalError(w, r, "create database", err)
			return
		}
		rec, err := deps.Store.Get(r.Context(), id)
		if err != nil {
			jsonError(w, "created but failed to read back",
				http.StatusInternalServerError)
			return
		}
		if err := runDatabaseCreateHook(
			r.Context(), deps, *rec,
		); err != nil {
			if deleteErr := deps.Store.Delete(r.Context(), id); deleteErr != nil {
				slog.Error("rollback failed managed database create",
					"database_id", id, "error", deleteErr)
			}
			internalError(w, r, "activate managed database", err)
			return
		}
		w.WriteHeader(http.StatusCreated)
		jsonResponse(w, dbRecordToMap(*rec))
	}
}

func updateManagedDBHandler(
	deps *DatabaseDeps,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			jsonError(w, "invalid database ID",
				http.StatusBadRequest)
			return
		}
		var req dbCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body",
				http.StatusBadRequest)
			return
		}
		oldRec, err := deps.Store.Get(r.Context(), id)
		if err != nil {
			jsonError(w, "database not found",
				http.StatusNotFound)
			return
		}
		if req.TrustLevel != oldRec.TrustLevel ||
			req.ExecutionMode != oldRec.ExecutionMode {
			jsonError(w,
				"trust_level and execution_mode must be changed in database Settings",
				http.StatusBadRequest)
			return
		}
		input := req.toInput()
		input.TrustLevel = oldRec.TrustLevel
		input.ExecutionMode = oldRec.ExecutionMode
		rec, err := applyDatabaseUpdate(
			r.Context(), deps, id, oldRec, input,
		)
		if err != nil {
			if errors.Is(err, store.ErrValidation) {
				jsonError(w, err.Error(),
					http.StatusBadRequest)
				return
			}
			if errors.Is(err, store.ErrNotFound) {
				jsonError(w, "database not found",
					http.StatusNotFound)
				return
			}
			internalError(w, r, "update database", err)
			return
		}
		jsonResponse(w, dbRecordToMap(*rec))
	}
}

func applyDatabaseUpdate(
	ctx context.Context,
	deps *DatabaseDeps,
	id int,
	oldRec *store.DatabaseRecord,
	input store.DatabaseInput,
) (*store.DatabaseRecord, error) {
	if deps.ApplyUpdate != nil {
		if oldRec == nil {
			return nil, store.ErrNotFound
		}
		return deps.ApplyUpdate(ctx, id, *oldRec, input)
	}
	if err := deps.Store.Update(ctx, id, input); err != nil {
		return nil, err
	}
	rec, err := deps.Store.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read back updated database: %w", err)
	}
	if oldRec != nil {
		if err := runDatabaseUpdateHook(ctx, deps, *oldRec, *rec); err != nil {
			return nil, err
		}
	}
	return rec, nil
}

func runDatabaseCreateHook(
	ctx context.Context, deps *DatabaseDeps, rec store.DatabaseRecord,
) error {
	if deps == nil || deps.OnCreate == nil {
		return nil
	}
	return deps.OnCreate(ctx, rec)
}

func runDatabaseUpdateHook(
	ctx context.Context, deps *DatabaseDeps,
	oldRec, newRec store.DatabaseRecord,
) error {
	if deps == nil || deps.OnUpdate == nil {
		return nil
	}
	return deps.OnUpdate(ctx, oldRec, newRec)
}

func deleteManagedDBHandler(
	deps *DatabaseDeps,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			jsonError(w, "invalid database ID",
				http.StatusBadRequest)
			return
		}
		rec, err := deps.Store.Get(r.Context(), id)
		if err != nil {
			jsonError(w, "database not found",
				http.StatusNotFound)
			return
		}
		if deps.ApplyDelete != nil {
			if err := deps.ApplyDelete(r.Context(), id, *rec); err != nil {
				internalError(w, r, "delete managed database", err)
				return
			}
		} else {
			if err := deps.Store.Delete(r.Context(), id); err != nil {
				jsonError(w, "failed to delete database",
					http.StatusInternalServerError)
				return
			}
			if err := runDatabaseDeleteTeardown(
				context.WithoutCancel(r.Context()), deps, rec.Name,
			); err != nil {
				internalError(w, r, "retire managed database", err)
				return
			}
		}
		jsonResponse(w, map[string]any{
			"ok": true, "id": id,
		})
	}
}

func runDatabaseDeleteTeardown(
	ctx context.Context, deps *DatabaseDeps, name string,
) error {
	if deps == nil || deps.Fleet == nil {
		return nil
	}
	return deps.Fleet.RemoveInstanceContext(ctx, name)
}

func testManagedDBHandler(
	deps *DatabaseDeps,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			jsonError(w, "invalid database ID",
				http.StatusBadRequest)
			return
		}

		var req dbCreateRequest
		hasPreviewBody := false
		if r.Body != nil {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				if !errors.Is(err, io.EOF) {
					jsonError(w, "invalid request body",
						http.StatusBadRequest)
					return
				}
			} else {
				hasPreviewBody = true
			}
		}

		rec, err := deps.Store.Get(r.Context(), id)
		if err != nil {
			jsonError(w, "database not found",
				http.StatusNotFound)
			return
		}

		fleetConnStr, fleetPassword, fleetFound :=
			fleetManagedDBConnection(deps, id, rec.Name)

		var connStr string
		if hasPreviewBody {
			if sameManagedHost(req.Host, rec.Host) {
				applyManagedTestDefaults(&req)
			} else {
				if !applySafePreviewHost(w, &req) {
					return
				}
			}
			fallbackPassword := fleetPassword
			if req.Password == "" && fallbackPassword == "" {
				storedConnStr, storeErr := deps.Store.GetConnectionString(
					r.Context(), id)
				if storeErr != nil {
					internalError(w, r,
						"read database credentials", storeErr)
					return
				}
				if parsed, parseErr := url.Parse(
					storedConnStr); parseErr == nil {
					fallbackPassword, _ = parsed.User.Password()
				}
			}
			connStr = buildManagedTestConnString(
				req, fallbackPassword)
		} else if fleetFound {
			connStr = fleetConnStr
		} else {
			connStr, err = deps.Store.GetConnectionString(
				r.Context(), id)
			if err != nil {
				internalError(w, r,
					"read database credentials", err)
				return
			}
		}
		result := testFromConnString(r.Context(), connStr)
		jsonResponse(w, result)
	}
}

func fleetManagedDBConnection(
	deps *DatabaseDeps, id int, name string,
) (connStr string, password string, ok bool) {
	if deps == nil || deps.Fleet == nil {
		return "", "", false
	}
	inst := deps.Fleet.GetInstanceByDatabaseID(id)
	if inst == nil && name != "" {
		inst = deps.Fleet.GetInstance(name)
	}
	if inst == nil {
		return "", "", false
	}
	return inst.Config.ConnString(), inst.Config.Password, true
}

func testConnectionPreviewHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req dbCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body",
				http.StatusBadRequest)
			return
		}

		// SSRF protection: reject private/internal IPs,
		// cloud metadata endpoints, and unresolvable hosts.
		// Resolve ONCE here and connect to the resolved IP to
		// prevent DNS-rebinding TOCTOU (a hostname with a short
		// TTL could resolve to a public IP during the check and
		// a private IP during pgx.Connect otherwise).
		if !applySafePreviewHost(w, &req) {
			return
		}
		u := buildManagedTestURL(req, req.Password)
		result := testFromConnString(r.Context(), u.String())
		jsonResponse(w, result)
	}
}

func applySafePreviewHost(
	w http.ResponseWriter, req *dbCreateRequest,
) bool {
	safeIP, res := resolveSafeHost(req.Host)
	switch res {
	case hostBlocked:
		jsonError(w,
			"connections to private/internal addresses "+
				"are not allowed",
			http.StatusBadRequest)
		return false
	case hostDNSFailed:
		jsonError(w,
			"could not resolve hostname",
			http.StatusBadRequest)
		return false
	}

	req.Host = safeIP
	applyManagedTestDefaults(req)
	return true
}

func applyManagedTestDefaults(req *dbCreateRequest) {
	if req.Port == 0 {
		req.Port = 5432
	}
	if req.SSLMode == "" {
		req.SSLMode = "require"
	}
}

func sameManagedHost(incoming, stored string) bool {
	return strings.EqualFold(
		strings.TrimSpace(incoming),
		strings.TrimSpace(stored),
	)
}

func buildManagedTestConnString(
	req dbCreateRequest, fallbackPassword string,
) string {
	password := req.Password
	if password == "" {
		password = fallbackPassword
	}
	return buildManagedTestURL(req, password).String()
}

func buildManagedTestURL(
	req dbCreateRequest, password string,
) *url.URL {
	ssl := req.SSLMode
	if ssl == "" {
		ssl = "require"
	}
	port := req.Port
	if port == 0 {
		port = 5432
	}
	return &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(req.Username, password),
		Host:   fmt.Sprintf("%s:%d", req.Host, port),
		Path:   req.DatabaseName,
		RawQuery: url.Values{
			"sslmode":         {ssl},
			"connect_timeout": {"10"},
		}.Encode(),
	}
}

// hostCheckResult describes why a host was blocked.
type hostCheckResult int

const (
	hostAllowed   hostCheckResult = iota
	hostBlocked                   // private/internal IP
	hostDNSFailed                 // DNS resolution failed
)

// checkHost resolves the host and returns whether it
// should be blocked for SSRF protection.
func checkHost(host string) hostCheckResult {
	// Block known metadata hostnames.
	if host == "169.254.169.254" ||
		host == "metadata.google.internal" {
		return hostBlocked
	}

	ip := net.ParseIP(host)
	if ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() ||
			ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() {
			return hostBlocked
		}
		return hostAllowed
	}

	// Resolve hostname and check all IPs. Fail closed on
	// DNS errors to prevent DNS rebinding attacks.
	addrs, err := net.LookupHost(host)
	if err != nil {
		return hostDNSFailed
	}
	for _, addr := range addrs {
		resolved := net.ParseIP(addr)
		if resolved == nil {
			continue
		}
		if resolved.IsLoopback() || resolved.IsPrivate() ||
			resolved.IsLinkLocalUnicast() ||
			resolved.IsLinkLocalMulticast() {
			return hostBlocked
		}
	}
	return hostAllowed
}

// isBlockedHost is a convenience wrapper for existing callers.
func isBlockedHost(host string) bool {
	return checkHost(host) != hostAllowed
}

// resolveSafeHost validates a hostname for SSRF and returns the
// specific IP that the caller should actually connect to. For a
// literal IP input it returns the input unchanged. For a hostname
// it resolves DNS once, blocks if any resolved IP is private/loopback/
// link-local, and otherwise returns the first allowed IP string. This
// eliminates the DNS-rebinding TOCTOU window between the safety check
// and pgx.Connect's own resolution.
func resolveSafeHost(host string) (string, hostCheckResult) {
	if host == "169.254.169.254" ||
		host == "metadata.google.internal" {
		return "", hostBlocked
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() ||
			ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() {
			return "", hostBlocked
		}
		return host, hostAllowed
	}
	addrs, err := net.LookupHost(host)
	if err != nil || len(addrs) == 0 {
		return "", hostDNSFailed
	}
	var firstAllowed string
	for _, addr := range addrs {
		resolved := net.ParseIP(addr)
		if resolved == nil {
			continue
		}
		if resolved.IsLoopback() || resolved.IsPrivate() ||
			resolved.IsLinkLocalUnicast() ||
			resolved.IsLinkLocalMulticast() {
			return "", hostBlocked
		}
		if firstAllowed == "" {
			firstAllowed = addr
		}
	}
	if firstAllowed == "" {
		return "", hostDNSFailed
	}
	return firstAllowed, hostAllowed
}

func importCSVHandler(
	deps *DatabaseDeps,
) http.HandlerFunc {
	const maxUpload = 5 << 20 // 5 MB total upload limit
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			jsonError(w, "invalid multipart form",
				http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			jsonError(w, "missing file field",
				http.StatusBadRequest)
			return
		}
		defer func() { _ = file.Close() }()

		user := UserFromContext(r.Context())
		createdBy := 0
		if user != nil {
			createdBy = user.ID
		}
		result := processCSVImport(
			r.Context(), deps.Store, file, createdBy)
		jsonResponse(w, result)
	}
}

// dbCreateRequest is the JSON body for create/update.
type dbCreateRequest struct {
	Name           string            `json:"name"`
	Host           string            `json:"host"`
	Port           int               `json:"port"`
	DatabaseName   string            `json:"database_name"`
	Username       string            `json:"username"`
	Password       string            `json:"password"`
	SSLMode        string            `json:"sslmode"`
	MaxConnections int               `json:"max_connections"`
	Tags           map[string]string `json:"tags"`
	TrustLevel     string            `json:"trust_level"`
	ExecutionMode  string            `json:"execution_mode"`
}

func (r *dbCreateRequest) toInput() store.DatabaseInput {
	maxConnections := r.MaxConnections
	if maxConnections == 0 {
		maxConnections = 2
	}
	return store.DatabaseInput{
		Name:           r.Name,
		Host:           r.Host,
		Port:           r.Port,
		DatabaseName:   r.DatabaseName,
		Username:       r.Username,
		Password:       r.Password,
		SSLMode:        r.SSLMode,
		MaxConnections: maxConnections,
		Tags:           r.Tags,
		TrustLevel:     r.TrustLevel,
		ExecutionMode:  r.ExecutionMode,
	}
}

func dbRecordToMap(rec store.DatabaseRecord) map[string]any {
	return map[string]any{
		"id":              rec.ID,
		"name":            rec.Name,
		"host":            rec.Host,
		"port":            rec.Port,
		"database_name":   rec.DatabaseName,
		"username":        rec.Username,
		"sslmode":         rec.SSLMode,
		"max_connections": rec.MaxConnections,
		"enabled":         rec.Enabled,
		"tags":            rec.Tags,
		"trust_level":     rec.TrustLevel,
		"execution_mode":  rec.ExecutionMode,
		"created_at":      rec.CreatedAt,
		"updated_at":      rec.UpdatedAt,
	}
}
