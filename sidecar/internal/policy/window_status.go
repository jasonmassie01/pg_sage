package policy

import "context"

// WindowReporter reports whether an unattended moderate action (such as an
// autonomous index build) is inside its maintenance window right now.
type WindowReporter interface {
	MaintenanceWindowOpen(context.Context, ActionRequest) (bool, error)
}

// MaintenanceWindowOpen evaluates the window exactly as the gate does for
// an unattended moderate action: trust.maintenance_window must be open and
// a standing-policy window must contain now. Missing or invalid policy
// fails closed.
func (gate *authorizationGate) MaintenanceWindowOpen(
	ctx context.Context, req ActionRequest,
) (bool, error) {
	runtime, err := gate.runtime(ctx, req)
	if err != nil {
		return false, err
	}
	doc, err := gate.policy(ctx, req)
	if err != nil {
		return false, err
	}
	if err := ValidateDocument(doc); err != nil {
		return false, err
	}
	return runtime.InConfiguredWindow && inAnyWindow(doc.MaintenanceWindows, gate.now()), nil
}
