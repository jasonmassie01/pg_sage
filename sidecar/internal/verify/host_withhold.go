package verify

import "strings"

// ReasonHostTelemetry: managed-cloud host telemetry (replica lag, storage
// runway, memory pressure) withholds an otherwise admitted build.
const ReasonHostTelemetry = "host_telemetry_withhold"

// applyHostWithhold turns an admission into a wait when host telemetry
// gives a reason; a blank reason is none. It never admits.
func applyHostWithhold(admission Admission, evidence LoadEvidence,
	record map[string]any) Admission {
	reasons := strings.TrimSpace(evidence.HostWithhold)
	if reasons == "" {
		return admission
	}
	record["host_withhold"] = reasons
	admission.OK, admission.Reason = false, ReasonHostTelemetry
	admission.Detail = "host telemetry: " + reasons
	return admission
}
