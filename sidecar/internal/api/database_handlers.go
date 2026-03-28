package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// DatabaseDeps holds dependencies for managed database handlers.
type DatabaseDeps struct {
	Store *store.DatabaseStore
	Fleet *fleet.DatabaseManager
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
		id, err := deps.Store.Create(
			r.Context(), input, createdBy)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec, err := deps.Store.Get(r.Context(), id)
		if err != nil {
			jsonError(w, "created but failed to read back",
				http.StatusInternalServerError)
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
		input := req.toInput()
		if err := deps.Store.Update(
			r.Context(), id, input,
		); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec, err := deps.Store.Get(r.Context(), id)
		if err != nil {
			jsonError(w, "updated but failed to read back",
				http.StatusInternalServerError)
			return
		}
		jsonResponse(w, dbRecordToMap(*rec))
	}
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
		if deps.Fleet != nil {
			deps.Fleet.RemoveInstance(rec.Name)
		}
		if err := deps.Store.Delete(r.Context(), id); err != nil {
			jsonError(w, "failed to delete database",
				http.StatusInternalServerError)
			return
		}
		jsonResponse(w, map[string]any{
			"ok": true, "id": id,
		})
	}
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
		connStr, err := deps.Store.GetConnectionString(
			r.Context(), id)
		if err != nil {
			jsonError(w, "database not found",
				http.StatusNotFound)
			return
		}
		result := testFromConnString(r.Context(), connStr)
		jsonResponse(w, result)
	}
}

func importCSVHandler(
	deps *DatabaseDeps,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		defer file.Close()

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
	Name          string            `json:"name"`
	Host          string            `json:"host"`
	Port          int               `json:"port"`
	DatabaseName  string            `json:"database_name"`
	Username      string            `json:"username"`
	Password      string            `json:"password"`
	SSLMode       string            `json:"sslmode"`
	Tags          map[string]string `json:"tags"`
	TrustLevel    string            `json:"trust_level"`
	ExecutionMode string            `json:"execution_mode"`
}

func (r *dbCreateRequest) toInput() store.DatabaseInput {
	return store.DatabaseInput{
		Name:          r.Name,
		Host:          r.Host,
		Port:          r.Port,
		DatabaseName:  r.DatabaseName,
		Username:      r.Username,
		Password:      r.Password,
		SSLMode:       r.SSLMode,
		Tags:          r.Tags,
		TrustLevel:    r.TrustLevel,
		ExecutionMode: r.ExecutionMode,
	}
}

func dbRecordToMap(rec store.DatabaseRecord) map[string]any {
	return map[string]any{
		"id":             rec.ID,
		"name":           rec.Name,
		"host":           rec.Host,
		"port":           rec.Port,
		"database_name":  rec.DatabaseName,
		"username":       rec.Username,
		"sslmode":        rec.SSLMode,
		"enabled":        rec.Enabled,
		"tags":           rec.Tags,
		"trust_level":    rec.TrustLevel,
		"execution_mode": rec.ExecutionMode,
		"created_at":     rec.CreatedAt,
		"updated_at":     rec.UpdatedAt,
	}
}
