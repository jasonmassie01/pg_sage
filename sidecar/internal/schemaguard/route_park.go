package schemaguard

// ParkedRoute is returned by a RemediationRouter that declines to act yet
// for a reason that is not a failure, such as a retention dry run still in
// its review window. The custodian records the remediation as parked with
// Reason and continues the scan; Err carries the underlying cause.
type ParkedRoute struct {
	Reason string
	Err    error
}

func (parked *ParkedRoute) Error() string {
	if parked.Err == nil {
		return parked.Reason
	}
	return parked.Reason + ": " + parked.Err.Error()
}

func (parked *ParkedRoute) Unwrap() error { return parked.Err }
