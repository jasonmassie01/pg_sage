package api

import (
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/tuning"
	"github.com/pg-sage/sidecar/internal/verify"
)

// calibrationOutcomesPerClass bounds the outcomes read per action class.
const calibrationOutcomesPerClass = 500

// tuningCalibrationHandler serves GET /api/v1/tuning/calibration?database=:
// the tuning agent's calibration (roadmap 2.2), per action class and
// prediction method, with the reliability bins that map predicted
// improvement to observed outcomes. It reads the same ledger window the
// agent reads.
func tuningCalibrationHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dbName, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		selected, ok := resolveSingleDatabaseRequestPool(mgr, dbName)
		if !ok || dbName == "" || dbName == "all" {
			jsonError(w, "database is required", http.StatusBadRequest)
			return
		}
		if selected.pool == nil {
			jsonError(w, "database not found", http.StatusNotFound)
			return
		}
		t := calibrationSettings(mgr)
		classes := []string{verify.ClassIndexCreate, verify.ClassIndexDrop, verify.ClassGUC,
			verify.ClassReloption, verify.ClassStatistics, verify.ClassQueryHint}
		since := time.Now().Add(-time.Duration(t.CalibrationWindowDays) * 24 * time.Hour)
		store := tuning.NewPostgresStore(selected.pool, 0, catalogread.Default())
		samples, err := store.Outcomes(r.Context(), classes, since,
			calibrationOutcomesPerClass)
		if err != nil {
			internalError(w, r, "read the tuning calibration", err)
			return
		}
		cal := tuning.Calibrate(samples, t.CalibrationMinOutcomes)
		jsonResponse(w, map[string]any{"database": selected.name,
			"min_outcomes": cal.MinOutcomes, "window_days": t.CalibrationWindowDays,
			"classes": cal.Classes, "excluded": cal.Excluded})
	}
}

// calibrationSettings reads the configured calibration window and minimum,
// falling back to the shipped defaults for values a config never set.
func calibrationSettings(mgr *fleet.DatabaseManager) config.TuningConfig {
	t := config.DefaultTuning()
	if mgr == nil || mgr.Config() == nil {
		return t
	}
	c := mgr.Config().Tuning
	if c.CalibrationMinOutcomes > 0 {
		t.CalibrationMinOutcomes = c.CalibrationMinOutcomes
	}
	if c.CalibrationWindowDays > 0 {
		t.CalibrationWindowDays = c.CalibrationWindowDays
	}
	return t
}
