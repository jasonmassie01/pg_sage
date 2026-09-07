package schemaguard

import "time"

func testPolicy() Policy {
	return Policy{
		AllowFKIndexApply:          true,
		AllowRetentionApply:        true,
		AllowRedundantIndexCleanup: true,
		OscillationLimit:           3,
	}
}

func testRequest(kind InvariantKind) Request {
	return Request{
		Invariant: Invariant{
			Kind:   kind,
			Schema: "public",
			Table:  "orders",
		},
		Policy: testPolicy(),
	}
}

func retentionRequest() Request {
	request := testRequest(InvariantUnboundedAppend)
	request.Contract = TableContract{
		AppendOnly:      true,
		RetentionWindow: 30 * 24 * time.Hour,
	}
	return request
}

func requirePlan(
	testingT interface {
		Helper()
		Fatalf(string, ...any)
	},
	decision Decision,
	wantClass RemediationClass,
	wantDisposition Disposition,
) {
	testingT.Helper()
	if decision.Class != wantClass || decision.Disposition != wantDisposition {
		testingT.Fatalf(
			"decision = %#v, want class=%q disposition=%q",
			decision, wantClass, wantDisposition,
		)
	}
}
