package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
)

// defaultReadinessTimeout bounds one /ready probe; Kubernetes' default probe
// timeout is 1 s, so the answer comes back inside it.
const defaultReadinessTimeout = 900 * time.Millisecond

// ReadinessProbe is what /ready checks. /health stays a static liveness
// answer; /ready says whether this sidecar can do its work: the config is
// loaded, the control database answers, and its sage schema is migrated.
type ReadinessProbe struct {
	ConfigLoaded bool
	// ControlDB pings the control database; nil means there is none.
	ControlDB func(context.Context) error
	// Schema checks the migrations; schema.ErrSchemaNotReady means not run.
	Schema  func(context.Context) error
	Timeout time.Duration
}

// ControlPoolReadiness probes pool, the database that holds sessions and
// sage state (the meta database, or the monitored one standalone).
func ControlPoolReadiness(cfg *config.Config, pool *pgxpool.Pool) ReadinessProbe {
	probe := ReadinessProbe{ConfigLoaded: cfg != nil}
	if pool == nil {
		return probe
	}
	probe.ControlDB = func(ctx context.Context) error {
		var one int
		return pool.QueryRow(ctx, "/* pg_sage readiness v1 */ SELECT 1").Scan(&one)
	}
	probe.Schema = func(ctx context.Context) error { return schema.CheckReady(ctx, pool) }
	return probe
}

type readinessResult struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// NewReadinessHandler answers GET/HEAD /ready with 200 when every check
// passes and 503 otherwise. The body names each check's state but never an
// error text: the endpoint is unauthenticated, and errors carry hosts and
// users. Failures are logged when readiness changes.
func NewReadinessHandler(probe ReadinessProbe) http.Handler {
	if probe.Timeout <= 0 {
		probe.Timeout = defaultReadinessTimeout
	}
	var wasReady atomic.Int32 // 0 unknown, 1 ready, 2 not ready
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), probe.Timeout)
		defer cancel()
		result, err := probe.run(ctx)
		state := int32(1)
		if result.Status != "ready" {
			state = 2
		}
		if wasReady.Swap(state) != state {
			logReadinessChange(result, err)
		}
		writeReadiness(w, r, result)
	})
}

// run evaluates the checks in order; the schema is not queried while the
// control database is unreachable. err joins the failures, for the log.
func (p ReadinessProbe) run(ctx context.Context) (readinessResult, error) {
	checks := map[string]string{"config": "ok", "control_db": "ok", "schema": "ok"}
	var failed error
	if !p.ConfigLoaded {
		checks["config"] = "not_loaded"
		failed = errors.New("configuration not loaded")
	}
	if p.ControlDB == nil {
		checks["control_db"], checks["schema"] = "absent", "skipped"
		failed = errors.Join(failed, errors.New("no control database"))
	} else if err := p.ControlDB(ctx); err != nil {
		checks["control_db"], checks["schema"] = "unreachable", "skipped"
		failed = errors.Join(failed, fmt.Errorf("control database: %w", err))
	} else {
		failed = errors.Join(failed, p.checkSchema(ctx, checks))
	}
	status := "ready"
	if failed != nil {
		status = "not_ready"
	}
	return readinessResult{Status: status, Checks: checks}, failed
}

func (p ReadinessProbe) checkSchema(ctx context.Context, checks map[string]string) error {
	if p.Schema == nil {
		return nil
	}
	err := p.Schema(ctx)
	switch {
	case err == nil:
	case errors.Is(err, schema.ErrSchemaNotReady):
		checks["schema"] = "not_migrated"
	default:
		checks["schema"] = "unknown"
	}
	return err
}

func logReadinessChange(result readinessResult, err error) {
	if err == nil {
		slog.Info("readiness: ready")
		return
	}
	slog.Warn("readiness: not ready", "checks", result.Checks, "error", err)
}

func writeReadiness(w http.ResponseWriter, r *http.Request, result readinessResult) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	code := http.StatusOK
	if result.Status != "ready" {
		code = http.StatusServiceUnavailable
	}
	w.WriteHeader(code)
	if r.Method == http.MethodHead {
		return
	}
	_ = json.NewEncoder(w).Encode(result)
}
