package agentdb

import "context"

// requireOwnedLiveResource proves pg_sage created the provider resource it
// is about to mutate: the row must be live, carry a recorded provider id,
// and a live creation receipt must name the same resource. A destroy is
// never issued against a name derived from the deployment id.
func (s *Store) requireOwnedLiveResource(ctx context.Context, dep Deployment) error {
	if !dep.LiveMode || dep.ProviderResourceID == "" {
		return ErrNotOwned
	}
	var owned bool
	err := s.pool.QueryRow(ctx, `/* pg_sage */
		SELECT EXISTS (
			SELECT 1 FROM sage.agent_db_creation_receipts
			WHERE deployment_id=$1 AND provider=$2
				AND provider_resource_id=$3 AND operation_mode='live'
		)`, dep.DeploymentID, dep.Provider, dep.ProviderResourceID,
	).Scan(&owned)
	if err != nil {
		return err
	}
	if !owned {
		return ErrNotOwned
	}
	return nil
}

// resumableTeardown reports whether a direct destroy may resume an already
// durable teardown operation instead of starting a new one.
func resumableTeardown(dep Deployment) bool {
	return dep.TeardownOperationID != "" &&
		(dep.ProvisioningStatus == "destroy_pending" ||
			dep.ProvisioningStatus == "destroying" ||
			dep.ProvisioningStatus == "status_unknown")
}

// validateDirectTeardownState checks every precondition prepareDirectTeardown
// enforces, without mutating, so callers can refuse before consuming a
// single-use authorization.
func (s *Store) validateDirectTeardownState(ctx context.Context, dep Deployment) error {
	if err := s.requireOwnedLiveResource(ctx, dep); err != nil {
		return err
	}
	if resumableTeardown(dep) {
		return nil
	}
	if dep.ProvisioningStatus != "available" && dep.ProvisioningStatus != "status_checked" {
		return ErrInvalid
	}
	if dep.TeardownOperationID != "" {
		return ErrConflict
	}
	return nil
}
