package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

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
	// Discovery serves the Discovered tab (market-06); nil leaves its
	// routes unregistered.
	Discovery *store.DiscoveryStore
}

// registerDatabaseRoutes registers /api/v1/databases/managed
// endpoints. All require admin role.
func registerDatabaseRoutes(
	mux *http.ServeMux, deps *DatabaseDeps,
) {
	list := http.HandlerFunc(
		listManagedDBHandler(deps))
	route(mux, "GET /api/v1/databases/managed", gateAdmin, list)

	create := http.HandlerFunc(
		createManagedDBHandler(deps))
	route(mux, "POST /api/v1/databases/managed", gateAdmin, create)

	importH := http.HandlerFunc(
		importCSVHandler(deps))
	route(mux, "POST /api/v1/databases/managed/import", gateAdmin, importH)

	getH := http.HandlerFunc(
		getManagedDBHandler(deps))
	route(mux, "GET /api/v1/databases/managed/{id}", gateAdmin, getH)

	updateH := http.HandlerFunc(
		updateManagedDBHandler(deps))
	route(mux, "PUT /api/v1/databases/managed/{id}", gateAdmin, updateH)

	deleteH := http.HandlerFunc(
		deleteManagedDBHandler(deps))
	route(mux, "DELETE /api/v1/databases/managed/{id}", gateAdmin, deleteH)

	testH := http.HandlerFunc(
		testManagedDBHandler(deps))
	route(mux, "POST /api/v1/databases/managed/{id}/test", gateAdmin, testH)

	testPreview := http.HandlerFunc(
		testConnectionPreviewHandler())
	route(mux, "POST /api/v1/databases/managed/test-connection", gateAdmin, testPreview)
	if deps.Discovery != nil {
		registerDiscoveredRoutes(mux, deps)
	}
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
			applyManagedDBCreate(w, r, deps, input, createdBy)
			return
		}
		storeManagedDBCreate(w, r, deps, input, createdBy)
	}
}

// applyManagedDBCreate creates and activates through the runtime hook.
func applyManagedDBCreate(
	w http.ResponseWriter, r *http.Request, deps *DatabaseDeps,
	input store.DatabaseInput, createdBy int,
) {
	rec, applyErr := deps.ApplyCreate(
		r.Context(), input, createdBy,
	)
	if applyErr != nil {
		if writeDatabaseInputError(w, applyErr) {
			return
		}
		internalError(w, r, "create and activate database", applyErr)
		return
	}
	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, dbRecordToMap(*rec))
}

// storeManagedDBCreate writes the record, then activates it; a failed
// activation deletes the record again.
func storeManagedDBCreate(
	w http.ResponseWriter, r *http.Request, deps *DatabaseDeps,
	input store.DatabaseInput, createdBy int,
) {
	id, err := deps.Store.Create(
		r.Context(), input, createdBy)
	if err != nil {
		if writeDatabaseInputError(w, err) {
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
			if writeDatabaseInputError(w, err) {
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

// dbCreateRequest is the JSON body for create/update.
type dbCreateRequest struct {
	resolvedIP     string
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
	// The action identity (security-09b) is write-only: nil leaves it
	// unchanged, "" removes it.
	ActionUsername *string `json:"action_username,omitempty"`
	ActionPassword string  `json:"action_password,omitempty"`
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
		Action:         r.actionInput(),
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
		// market-06: empty for manual and CSV members.
		"discovered_source": rec.DiscoveredSource,
		"owner":             rec.Owner,
		"expires_at":        rec.ExpiresAt,
		"adopted_at":        rec.AdoptedAt,
		"action_identity":   actionIdentityView(rec),
	}
}
