package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/value"
)

type valueReader interface {
	Get(context.Context, value.Filter) (value.Report, error)
}

// fleetValueReader aggregates the value ledger of every fleet instance
// (D3) and prices the instances from cfg's cost.rates (market-04). A nil
// manager yields no reader, so the route fails closed.
func fleetValueReader(mgr *fleet.DatabaseManager, cfg *config.Config) valueReader {
	if mgr == nil {
		return nil
	}
	return newFleetValue(mgr, cfg)
}

func valueHandler(reader valueReader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reader == nil {
			jsonError(w, "value service unavailable", http.StatusServiceUnavailable)
			return
		}
		filter, err := valueFilterFromRequest(r)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		report, err := reader.Get(r.Context(), filter)
		if errors.Is(err, value.ErrUnknownDatabase) {
			jsonError(w, "database not found", http.StatusNotFound)
			return
		}
		if err != nil {
			slog.Error("read value report", "error", err)
			jsonError(w, "unable to load value", http.StatusInternalServerError)
			return
		}
		resp := valueResponse{Report: report, Cost: costSection(r.Context(), reader, filter)}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			slog.Error("encode value report", "error", err)
		}
	})
}

func valueFilterFromRequest(r *http.Request) (value.Filter, error) {
	filter := value.Filter{Database: r.URL.Query().Get("database")}
	if err := validateDatabaseParam(filter.Database); err != nil {
		return value.Filter{}, err
	}
	var err error
	if raw := r.URL.Query().Get("since"); raw != "" {
		filter.Since, err = parseValueDate(raw)
		if err != nil {
			return value.Filter{}, err
		}
	}
	if raw := r.URL.Query().Get("until"); raw != "" {
		untilDay, err := parseValueDate(raw)
		if err != nil {
			return value.Filter{}, err
		}
		if !filter.Since.IsZero() && untilDay.Before(filter.Since) {
			return value.Filter{}, &valueRangeError{}
		}
		// until names the last day of the report: the bound is the next
		// midnight, exclusive, so every moment of that day is counted.
		filter.Until = untilDay.AddDate(0, 0, 1)
	}
	return filter, nil
}

func parseValueDate(raw string) (time.Time, error) {
	parsed, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return time.Time{}, &valueDateError{value: raw}
	}
	return parsed.UTC(), nil
}

type valueDateError struct{ value string }

func (e *valueDateError) Error() string {
	return "invalid value date " + e.value + "; expected YYYY-MM-DD"
}

type valueRangeError struct{}

func (*valueRangeError) Error() string { return "until must be on or after since" }
