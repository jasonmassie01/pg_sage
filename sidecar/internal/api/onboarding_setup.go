package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/onboarding"
)

// The setup wizard (ux-04b): the checklist's Model step and the first-done
// times of the wizard's model, channel and autonomy steps, recorded with
// POST /api/v1/onboarding/setup.

// setupTimes is when each wizard step was first finished (null: not yet).
type setupTimes struct {
	ModelConfiguredAt   *time.Time `json:"model_configured_at"`
	ChannelConfiguredAt *time.Time `json:"channel_configured_at"`
	AutonomyChosenAt    *time.Time `json:"autonomy_chosen_at"`
}

func utcOrNil(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func setupTimesOf(st *onboarding.State) setupTimes {
	if st == nil {
		return setupTimes{}
	}
	return setupTimes{ModelConfiguredAt: utcOrNil(st.ModelConfiguredAt),
		ChannelConfiguredAt: utcOrNil(st.ChannelConfiguredAt),
		AutonomyChosenAt:    utcOrNil(st.AutonomyChosenAt)}
}

// modelStatusFor is the model a database's LLM features use: its own
// general client when it has one ("<database>/general"), else the process
// one. No registry, or no general client, is a known unconfigured model.
func modelStatusFor(reg LLMBudgetRegistry, database string) *onboarding.ModelStatus {
	if reg == nil {
		return &onboarding.ModelStatus{}
	}
	all := reg.TokenStatus()
	c, ok := all[database+"/general"]
	if !ok {
		c, ok = all["general"]
	}
	if !ok {
		return &onboarding.ModelStatus{}
	}
	s := onboarding.ModelStatusOf(onboarding.ModelClient{Enabled: c.Enabled,
		ConfigurationError: c.ConfigurationError, LastErrorClass: c.LastErrorClass,
		LastErrorAt: c.LastErrorAt, LastSuccessAt: c.LastSuccessAt})
	return &s
}

type setupRequest struct {
	Database string `json:"database"`
	Step     string `json:"step"`
}

const maxSetupBody = 4 << 10

// readSetupRequest decodes and validates the body, writing a 400 when it
// cannot be served.
func readSetupRequest(w http.ResponseWriter, r *http.Request) (setupRequest, bool) {
	var req setupRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSetupBody))
	if err := dec.Decode(&req); err != nil {
		jsonError(w, "body must be JSON: {\"database\": ..., \"step\": ...}",
			http.StatusBadRequest)
		return req, false
	}
	switch req.Step {
	case onboarding.SetupModel, onboarding.SetupChannel, onboarding.SetupAutonomy:
	default:
		jsonError(w, fmt.Sprintf("unknown setup step %q (want model, channel or autonomy)",
			req.Step), http.StatusBadRequest)
		return req, false
	}
	if req.Database == "" || req.Database == "all" {
		jsonError(w, "choose one database (\"database\")", http.StatusBadRequest)
		return req, false
	}
	if err := validateDatabaseParam(req.Database); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return req, false
	}
	return req, true
}

// unfinishedSetup says why step is not finished yet ("" when it is): the
// model must have answered, and a usable channel must exist. Choosing an
// autonomy level is the config PUT itself, so it has no check here.
func unfinishedSetup(ctx context.Context, reader onboardingReader, database,
	step string) string {
	switch step {
	case onboarding.SetupModel:
		m := reader.Model(ctx, database)
		switch {
		case m == nil || !m.Configured:
			return "no model is set: set the provider endpoint and key first"
		case !m.Answered:
			return "the model has not answered: test it, then finish this step"
		}
	case onboarding.SetupChannel:
		if on := reader.Notifications(ctx); on == nil || !*on {
			return "no usable notification channel: add one and send a test first"
		}
	}
	return ""
}

// onboardingSetupHandler records that a wizard step was finished, once
// its result exists; a repeat keeps the first time.
func onboardingSetupHandler(reader onboardingReader, now func() time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reader == nil {
			jsonError(w, "onboarding unavailable", http.StatusServiceUnavailable)
			return
		}
		req, ok := readSetupRequest(w, r)
		if !ok {
			return
		}
		if _, found := reader.Names(req.Database); !found {
			jsonError(w, "database not found", http.StatusNotFound)
			return
		}
		if why := unfinishedSetup(r.Context(), reader, req.Database, req.Step); why != "" {
			jsonError(w, why, http.StatusConflict)
			return
		}
		recorded, err := reader.RecordSetup(r.Context(), req.Database, req.Step, now())
		if errors.Is(err, onboarding.ErrNotInitialized) {
			jsonError(w, "onboarding of this database has not started yet; retry "+
				"once it is connected", http.StatusConflict)
			return
		}
		if err != nil {
			internalError(w, r, "record setup step", err)
			return
		}
		jsonResponse(w, map[string]any{"database": req.Database, "step": req.Step,
			"recorded": recorded})
	})
}

func (f *fleetOnboardingReader) Model(_ context.Context, name string) *onboarding.ModelStatus {
	return modelStatusFor(f.budgets, name)
}

func (f *fleetOnboardingReader) RecordSetup(ctx context.Context, name, step string,
	at time.Time) (bool, error) {
	inst, err := f.instance(name)
	if err != nil {
		return false, err
	}
	return onboarding.RecordSetupStep(ctx, inst.Pool, name, step, at)
}
