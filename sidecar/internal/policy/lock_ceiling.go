package policy

// EffectiveLockTimeoutMS combines the policy's lock_duration_ceiling_ms with
// safety.lock_timeout_ms for DDL that runs inside a transaction: the
// tighter bound wins. PostgreSQL reads lock_timeout = 0 as "no timeout", so
// a non-positive value on either side means that side sets no bound.
//
// lock_timeout bounds the wait to acquire a lock. Capping it matters most
// for in-transaction ALTER TABLE, whose queued ACCESS EXCLUSIVE request
// stalls every later query on the table. CONCURRENTLY builds wait on older
// transactions by design; the executor keeps the safety value for them
// because a short cap would mostly leave INVALID indexes behind.
func EffectiveLockTimeoutMS(ceilingMS, safetyMS int64) int64 {
	switch {
	case ceilingMS <= 0:
		return safetyMS
	case safetyMS <= 0:
		return ceilingMS
	}
	return min(ceilingMS, safetyMS)
}

// withLockCeiling carries the policy ceiling on an execute verdict so the
// executor can cap lock_timeout for in-transaction DDL.
func withLockCeiling(doc Document, decision Decision) Decision {
	if decision.Verdict == VerdictExecute && doc.LockDurationCeilingMS > 0 {
		decision.LockCeilingMS = doc.LockDurationCeilingMS
	}
	return decision
}
