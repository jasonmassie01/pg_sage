package rca

import (
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/sanitize"
	"github.com/pg-sage/sidecar/internal/signalid"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// holderIncident names the oldest holder the rules allow (Branch 1, and
// Branch 2 for a prepared transaction in the holder read). Its incidents
// are live evidence: an open incident takes the newest holder state (a
// terminate once the idle minutes pass, or the next holder).
func (e *Engine) holderIncident(sig *Signal, curr *collector.Snapshot,
	r vacuumRules) (Incident, bool) {
	tables := blockedTables(sig)
	for _, c := range xminCandidates(curr) {
		if !r.qualifies(c, tables) {
			continue
		}
		var inc Incident
		switch c.holder.Kind {
		case probes.HolderSession:
			inc = sessionHolderIncident(sig, curr, c, r)
		case probes.HolderPreparedXact:
			inc = preparedHolderIncident(sig, curr, c.holder)
		case probes.HolderStandby:
			inc = standbyHolderIncident(sig, curr, c.holder)
		default:
			inc = slotHolderIncident(sig, curr, c.holder)
		}
		inc.liveEvidence = true
		return inc, true
	}
	return Incident{}, false
}

func holderLink(desc, evidence string, b *BlockerIdentity) []ChainLink {
	return []ChainLink{{Order: 1, Signal: signalid.VacuumBlocked, Description: desc,
		Evidence: evidence, Blocker: b}}
}

// sessionHolderIncident names a session. pg_terminate_backend is proposed
// only for a holder from the full read, idle in transaction for at least
// the terminate minutes, and not a protected application; otherwise the
// incident carries a read-only diagnostic.
func sessionHolderIncident(sig *Signal, curr *collector.Snapshot, c xminCandidate,
	r vacuumRules) Incident {
	h := c.holder
	state := h.State
	if c.lock != nil && c.lock.State != nil {
		state = *c.lock.State
	}
	age := int64(h.XminAge)
	idle := strings.HasPrefix(state, "idle in transaction")
	evidence := fmt.Sprintf("PID %d in state: %s; xmin age %d, transaction open %s",
		h.PID, state, age, secondsText(h.XactAgeS))
	idleFor, known := idleDuration(curr, c)
	if known {
		evidence += ", in this state " + idleFor.String()
	}
	// The "Idle-in-transaction PID" wording, which routes the case to the
	// idle-in-transaction playbook, is kept for a holder this tree would
	// terminate; any other holder reads as a session in its state. Cases
	// builds no signal from the text (playbooks-01), so the terminate stays
	// this incident's recommended SQL.
	root := fmt.Sprintf("Session PID %d (%s) holding oldest xmin (age %d), "+
		"blocking autovacuum", h.PID, state, age)
	sql, risk := sessionDiagnosticSQL(h.PID), "safe"
	if idle && c.complete && known && idleFor >= r.idleTerminate && !r.protected(h.Application) {
		root = fmt.Sprintf("Idle-in-transaction PID %d holding oldest xmin (age %d), "+
			"blocking autovacuum", h.PID, age)
		sql = fmt.Sprintf("SELECT pg_terminate_backend(%d); -- idle-in-tx xmin holder", h.PID)
		risk = "high_risk"
	}
	return buildIncident(curr.CollectedAt, sig.Severity, []string{signalid.VacuumBlocked}, root,
		holderLink("Session holding the oldest snapshot or XID prevents dead-tuple "+
			"cleanup", evidence, sessionBlocker(curr, c, state)),
		blockedTables(sig), sql, risk)
}

// idleDuration is how long the session has been in its state. With the
// holder's server-side transaction age and the lock row's xact_start it
// needs no sidecar clock; otherwise it is measured from the snapshot.
func idleDuration(curr *collector.Snapshot, c xminCandidate) (time.Duration, bool) {
	if c.lock == nil || c.lock.StateChange == nil {
		return 0, false
	}
	sc := *c.lock.StateChange
	if c.complete && c.lock.XactStart != nil && c.holder.XactAgeS > 0 {
		xact := time.Duration(c.holder.XactAgeS * float64(time.Second))
		return (xact - sc.Sub(*c.lock.XactStart)).Round(time.Microsecond), true
	}
	return curr.CollectedAt.Sub(sc), true
}

// sessionBlocker is the structured identity of a session holder, so the
// incident resolves when that session ends (and Cases reads structure).
func sessionBlocker(curr *collector.Snapshot, c xminCandidate, state string) *BlockerIdentity {
	h := c.holder
	b := &BlockerIdentity{PID: int(h.PID), BackendStart: h.BackendStart, State: state,
		XminAge: int64(h.XminAge), HolderKind: probes.HolderSession}
	switch {
	case c.lock != nil && c.lock.XactStart != nil:
		b.XactStart = *c.lock.XactStart
	case h.XactAgeS > 0:
		b.XactStart = curr.CollectedAt.Add(
			-time.Duration(h.XactAgeS * float64(time.Second))).Round(time.Microsecond)
	}
	if b.BackendStart.IsZero() && c.lock != nil && c.lock.BackendStart != nil {
		b.BackendStart = *c.lock.BackendStart
	}
	return b
}

func sessionDiagnosticSQL(pid int64) string {
	return fmt.Sprintf("SELECT pid, state, xact_start, state_change, "+
		"age(backend_xmin) AS xmin_age, age(backend_xid) AS xid_age, application_name, "+
		"left(query, 200) AS query FROM pg_stat_activity WHERE pid = %d;", pid)
}

func secondsText(s float64) string {
	return time.Duration(s * float64(time.Second)).Round(time.Second).String()
}

// preparedHolderIncident names a prepared transaction from the holder
// read. Without the collector's row there is no GID to quote: the
// incident carries a diagnostic by the gid's hash.
func preparedHolderIncident(sig *Signal, curr *collector.Snapshot,
	h probes.XminHolder) Incident {
	if p, ok := preparedByHash(curr, h.Name); ok {
		return preparedIncident(sig, curr, p, int64(h.XminAge))
	}
	age := int64(h.XminAge)
	b := &BlockerIdentity{State: "prepared", XminAge: age,
		HolderKind: probes.HolderPreparedXact, HolderName: h.Name}
	return buildIncident(curr.CollectedAt, "critical", []string{signalid.VacuumBlocked},
		fmt.Sprintf("Prepared transaction (gid md5 %s) holding oldest xmin (age %d), "+
			"blocking autovacuum", h.Name, age),
		holderLink("Prepared transaction holds xmin indefinitely -- invisible to "+
			"pg_stat_activity", fmt.Sprintf("gid md5=%s xid_age=%d prepared %s ago",
			h.Name, age, secondsText(h.XactAgeS)), b),
		blockedTables(sig),
		"SELECT gid, prepared, owner, age(transaction) AS xid_age FROM pg_prepared_xacts "+
			"WHERE database = current_database() AND md5(gid) = "+
			sanitize.QuoteLiteral(h.Name)+";", "safe")
}

// preparedIncident is Branch 2: a prepared transaction of this database
// past the prepared floor. ROLLBACK PREPARED is proposed for approval with
// the GID as a quoted literal.
func preparedIncident(sig *Signal, curr *collector.Snapshot,
	p collector.PreparedTransaction, age int64) Incident {
	b := &BlockerIdentity{State: "prepared", XminAge: age, XactStart: p.Prepared,
		HolderKind: probes.HolderPreparedXact, HolderName: gidHash(p.GID)}
	return buildIncident(curr.CollectedAt, "critical", []string{signalid.VacuumBlocked},
		fmt.Sprintf("Orphaned prepared transaction %q holding xmin (age %d), "+
			"blocking autovacuum", p.GID, age),
		holderLink("Prepared transaction holds xmin indefinitely -- invisible to "+
			"pg_stat_activity", fmt.Sprintf("gid=%q owner=%s prepared=%s xid_age=%d",
			p.GID, p.Owner, p.Prepared.Format("2006-01-02 15:04:05"), age), b),
		blockedTables(sig), "ROLLBACK PREPARED "+gidLiteral(p.GID)+";", "high_risk")
}

// gidLiteral quotes a prepared transaction's GID as a string literal. A
// GID with a backslash becomes an escape string with the backslashes
// doubled (as quote_literal does), so the literal reads the same, and a
// quote cannot end it, whatever standard_conforming_strings is.
func gidLiteral(gid string) string {
	if !strings.Contains(gid, `\`) {
		return sanitize.QuoteLiteral(gid)
	}
	return "E" + sanitize.QuoteLiteral(strings.ReplaceAll(gid, `\`, `\\`))
}

// standbyHolderIncident names a standby's hot_standby_feedback through its
// walsender; pg_sage never signals it.
func standbyHolderIncident(sig *Signal, curr *collector.Snapshot,
	h probes.XminHolder) Incident {
	age := int64(h.XminAge)
	b := &BlockerIdentity{PID: int(h.PID), BackendStart: h.BackendStart, State: h.State,
		XminAge: age, HolderKind: probes.HolderStandby}
	return buildIncident(curr.CollectedAt, sig.Severity, []string{signalid.VacuumBlocked},
		fmt.Sprintf("Hot standby feedback through walsender PID %d holding oldest xmin "+
			"(age %d), blocking autovacuum", h.PID, age),
		holderLink("A standby's hot_standby_feedback holds back dead-tuple cleanup; end "+
			"the standby's long query or review its feedback",
			fmt.Sprintf("walsender PID %d (%s) xmin age %d", h.PID, h.Application, age), b),
		blockedTables(sig), fmt.Sprintf("SELECT pid, application_name, client_addr, state, "+
			"backend_xmin, age(backend_xmin) AS xmin_age FROM pg_stat_replication "+
			"WHERE pid = %d;", h.PID), "safe")
}

// slotHolderIncident names a replication slot (its xmin, or its
// catalog_xmin for a catalog table); pg_sage never advances or drops one.
func slotHolderIncident(sig *Signal, curr *collector.Snapshot,
	h probes.XminHolder) Incident {
	age := int64(h.XminAge)
	b := &BlockerIdentity{State: h.State, XminAge: age, HolderKind: h.Kind,
		HolderName: h.Name}
	root := fmt.Sprintf("Replication slot %q holding oldest xmin (age %d), "+
		"blocking autovacuum", h.Name, age)
	if h.Kind == probes.HolderSlotCatalog {
		root = fmt.Sprintf("Logical replication slot %q holding oldest catalog_xmin "+
			"(age %d), blocking catalog vacuum", h.Name, age)
	}
	return buildIncident(curr.CollectedAt, sig.Severity, []string{signalid.VacuumBlocked}, root,
		holderLink("A replication slot holds back dead-tuple cleanup; confirm its "+
			"consumer is alive before advancing or dropping it",
			fmt.Sprintf("slot %q (%s) %s age %d", h.Name, h.State, h.Kind, age), b),
		blockedTables(sig), "SELECT slot_name, slot_type, active, xmin, catalog_xmin, "+
			"age(xmin) AS xmin_age, age(catalog_xmin) AS catalog_xmin_age "+
			"FROM pg_replication_slots WHERE slot_name = "+sanitize.QuoteLiteral(h.Name)+";",
		"safe")
}
