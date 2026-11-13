package api

import (
	"errors"
	"net/http"

	"github.com/pg-sage/sidecar/internal/store"
)

// notificationWriteError answers a failed channel or rule write: 409
// managed_by_config for a row the config file's alerting: block owns
// (alerting-06b), 404 for a missing row, 400 for invalid input and a
// logged 500 otherwise.
func notificationWriteError(
	w http.ResponseWriter, r *http.Request, op, notFound string, err error,
) {
	switch {
	case errors.Is(err, store.ErrManagedByConfig):
		jsonError(w, store.ErrManagedByConfig.Error(), http.StatusConflict)
	case errors.Is(err, store.ErrNotFound):
		jsonError(w, notFound, http.StatusNotFound)
	case errors.Is(err, store.ErrValidation):
		jsonError(w, err.Error(), http.StatusBadRequest)
	default:
		internalError(w, r, op, err)
	}
}
