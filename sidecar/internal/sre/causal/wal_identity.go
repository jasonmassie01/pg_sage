package causal

import "github.com/pg-sage/sidecar/internal/sre/probes"

// walComparable checks that the WAL family's first and last samples come
// from the same server incarnation (CHECK-07). The slots, the WAL volume
// and the archiver are read together in each step, so a restart or a
// failover seen by either the WAL or the slot samples freezes every
// series to its last sample: what the server shows now is still an
// observation, but no growth, rate or rising count is computed across
// the change. Each series whose identity changed is stated as missing.
func walComparable(slots, wal, arch *walEvidence) (bool, []Missing) {
	var missing []Missing
	walReason := pairChange(*wal, walIdentity)
	if walReason != "" {
		missing = append(missing, Missing{ProbeID: probes.WALCheckpoint, Reason: walReason})
	}
	slotReason := pairChange(*slots, slotIdentity)
	if slotReason != "" {
		missing = append(missing, Missing{ProbeID: probes.ReplicationSlots,
			Reason: slotReason})
	}
	if walReason == "" && slotReason == "" {
		return false, nil
	}
	for _, w := range []*walEvidence{slots, wal, arch} {
		if w.n > 1 {
			w.first, w.n = w.last, 1
		}
	}
	return true, missing
}

// pairChange is why a series' first and last samples cannot be compared,
// or "" when they can or when either sample does not say which server
// produced it (an empty result names no server).
func pairChange(w walEvidence,
	identityOf func(Observation) (probes.ServerIdentity, bool)) string {
	if w.n < 2 {
		return ""
	}
	a, okA := identityOf(w.first)
	b, okB := identityOf(w.last)
	if !okA || !okB {
		return ""
	}
	return identityChange(a, b)
}

func walIdentity(o Observation) (probes.ServerIdentity, bool) {
	s, err := probes.WALStats(o.Result)
	return s.Identity, err == nil
}

func slotIdentity(o Observation) (probes.ServerIdentity, bool) {
	ss, err := probes.Slots(o.Result)
	if err != nil || len(ss) == 0 {
		return probes.ServerIdentity{}, false
	}
	return ss[0].Identity, true
}
