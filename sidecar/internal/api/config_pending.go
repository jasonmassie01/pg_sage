package api

import (
	"github.com/pg-sage/sidecar/internal/config"
)

// pendingEntry is one saved setting that is not in force yet (config-09):
// the value the process runs with, the value it will run with after a
// restart, and the key's effective lifecycle. A secret, or a value that
// may carry a credential or cannot be shown as a scalar, is listed by key
// alone with Hidden set.
type pendingEntry struct {
	Key       string `json:"key"`
	Active    any    `json:"active,omitempty"`
	Desired   any    `json:"desired,omitempty"`
	Hidden    bool   `json:"hidden,omitempty"`
	Lifecycle string `json:"lifecycle"`
}

// pendingEntries lists the controller's pending-restart keys with their
// active and desired values. It never returns nil, so the API sends [].
func pendingEntries(controller *config.ConfigController) []pendingEntry {
	entries := []pendingEntry{}
	if controller == nil {
		return entries
	}
	active := controller.Active().Config
	desired := controller.Desired().Config
	for _, key := range controller.PendingRestart() {
		entries = append(entries, newPendingEntry(controller, key, active, desired))
	}
	return entries
}

func newPendingEntry(
	controller *config.ConfigController, key string, active, desired *config.Config,
) pendingEntry {
	entry := pendingEntry{Key: key, Lifecycle: string(config.LifecycleRestart)}
	if lifecycle, ok := controller.EffectiveLifecycle(key); ok {
		entry.Lifecycle = string(lifecycle)
	}
	activeDoc, okActive := config.DescribeField(active, key)
	desiredDoc, okDesired := config.DescribeField(desired, key)
	if !okActive || !okDesired || !activeDoc.HasValue || !desiredDoc.HasValue {
		entry.Hidden = true
		return entry
	}
	entry.Active, entry.Desired = activeDoc.Value, desiredDoc.Value
	return entry
}

// restartSupported reports whether POST /api/v1/restart can restart the
// process: main wires the hook only under a declared supervisor
// (SAGE_SUPERVISED=1); otherwise the route answers 501.
func restartSupported() bool {
	return restartFunc != nil
}
