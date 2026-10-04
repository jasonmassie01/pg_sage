package policy

// Shadow mode (roadmap 1.4): when the earned ledger keeps a governed
// class from running unattended, the decision also says what the gate
// would have decided had the class been trusted at L3, from the same
// inputs (no extra reads). The executor records it with its shadow
// decision; it never changes the verdict.

// TrustedVerdict is the gate's verdict had a request's ledger pair been
// trusted at L3: execute, or a handoff the ceiling or an L3 condition
// still requires. Granted and Level are the ledger's answer at the time.
type TrustedVerdict struct {
	Verdict Verdict
	Reason  Reason
	Detail  string
	Granted int
	Level   int
}

// trustedVerdict is base judged at L3: the ceiling's own approval stays,
// an unmet L3 condition (one target, the window) makes it a handoff,
// otherwise it executes. Nil when the pair is already trusted (L3).
func (gate *authorizationGate) trustedVerdict(doc Document, runtime RuntimeState,
	req ActionRequest, base Decision, limit AutonomyLimit, level int) *TrustedVerdict {
	if level >= autonomyExecuteLevel {
		return nil
	}
	t := &TrustedVerdict{Verdict: VerdictExecute, Reason: ReasonAutonomyL3,
		Granted: limit.Granted, Level: level}
	if base.Verdict == VerdictQueueApproval {
		t.Verdict, t.Reason, t.Detail = VerdictQueueApproval, base.Reason, base.Detail
		return t
	}
	if why := l3Blocker(doc, runtime, req, gate.now(), gate.selfGoverned(req)); why != "" {
		t.Verdict, t.Reason, t.Detail = VerdictQueueApproval, ReasonAutonomyHandoff, why
	}
	return t
}

func withTrusted(d Decision, trusted *TrustedVerdict) Decision {
	d.Trusted = trusted
	return d
}
