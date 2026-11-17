package config

import (
	"fmt"
	"math"
	"strings"

	"github.com/pg-sage/sidecar/internal/provider"
)

// The rule table's thresholds.
const (
	sharedBuffersLow    = 0.15 // of memory: below is a contradiction
	sharedBuffersHigh   = 0.40 // of memory: above is a contradiction
	sharedBuffersTarget = 0.25 // of memory: the documented starting point
	// minSpillBytesPerHour is the temp-file rate (inclusive) that says
	// work_mem is too small for the workload's sorts and hashes.
	minSpillBytesPerHour = 256 << 20
	// workMemShare is the share of memory every connection's work_mem may
	// take at once: the cap on a raise is memory * share / max_connections.
	workMemShare = 0.25
	// slowStorageCost is the random_page_cost at or above which a planner
	// still assumes spinning disks.
	slowStorageCost = 2.0
	ssdPageCost     = 1.1
	pg18IOConcurr   = 16
	// idleTimeoutMs is the advised idle-in-transaction timeout: the upper
	// end of pgconf's 5-30min guidance (the pack has no per-session idle
	// times to size it closer).
	idleTimeoutMs = 30 * 60_000
)

const gibibyte = 1 << 30

// rule is one row of the rule table: the setting, what contradicts it,
// and the judge that emits a candidate (or notes why it cannot judge).
type rule struct {
	setting, against string
	judge            func(*builder)
}

// RuleRow is one row of the rule table as docs/review.md lists it.
type RuleRow struct{ Setting, Against string }

// RuleTable is the rule table's rows, in the order the rules run.
func RuleTable() []RuleRow {
	out := make([]RuleRow, len(rules))
	for i, r := range rules {
		out[i] = RuleRow{Setting: r.setting, Against: r.against}
	}
	return out
}

// rules is the rule table (docs/review.md lists it).
var rules = []rule{
	{"shared_buffers", "the server's memory", (*builder).sharedBuffers},
	{"work_mem", "the temp files written per hour", (*builder).workMem},
	{"max_wal_size", "requested against timed checkpoints and the WAL rate",
		(*builder).maxWALSize},
	{"random_page_cost", "the storage class", (*builder).randomPageCost},
	{"effective_io_concurrency, io_method", "PostgreSQL 18's asynchronous I/O",
		(*builder).pg18IO},
	{"idle_in_transaction_session_timeout", "sessions idle in a transaction",
		(*builder).idleInTransaction},
}

func (b *builder) sharedBuffers() {
	sb, ok := b.v.value("shared_buffers")
	if !ok || !b.v.memoryOK {
		return
	}
	share := sb / b.v.memory
	if share >= sharedBuffersLow && share <= sharedBuffersHigh {
		return
	}
	value, _ := Clamp("shared_buffers", b.v.memory*sharedBuffersTarget)
	if parsed("shared_buffers", value) == sb {
		return
	}
	b.emit(finding{kind: KindSharedBuffers, setting: "shared_buffers", value: value,
		evidence: append(b.v.cite("shared_buffers"), memoryFactID),
		reason: fmt.Sprintf("shared_buffers is %.1f%% of the server's %s of memory, "+
			"outside 15-40%%; 25%% (%s) is the documented starting point.", share*100,
			renderBytes(b.v.memory), value),
		metric: "shared_blks_read", direction: "down",
		quantities: map[string]float64{"memory_bytes": b.v.memory, "current_bytes": sb,
			"proposed_bytes": parsed("shared_buffers", value)}})
}

func (b *builder) workMem() {
	wm, ok := b.v.value("work_mem")
	temp, known := measure(b.v.measures.TempBytesPerHour)
	if !ok || !known || temp < minSpillBytesPerHour {
		if ok && !known {
			b.cov.Note("work_mem not judged: temp_bytes_per_hour is unknown")
		}
		return
	}
	want, evidence := 2*wm, b.v.cite("work_mem")
	if conns, ok := b.v.value("max_connections"); ok && conns > 0 && b.v.memoryOK {
		evidence = append(append(evidence, b.v.cite("max_connections")...), memoryFactID)
		want = math.Min(want, b.v.memory*workMemShare/conns)
	}
	value, _ := Clamp("work_mem", want)
	proposed := parsed("work_mem", value)
	if !(proposed > wm) || want <= wm {
		b.cov.Note("work_mem not raised: at %s it is at its memory cap (%.0f%% of memory "+
			"over max_connections) or its maximum", renderBytes(wm), workMemShare*100)
		return
	}
	b.emit(finding{kind: KindWorkMem, setting: "work_mem", value: value, evidence: evidence,
		reason: fmt.Sprintf("Sorts and hashes wrote %s of temp files an hour with work_mem "+
			"at %s; doubling it (within the memory cap) keeps more of them in memory.",
			renderBytes(math.Floor(temp)), renderBytes(wm)),
		metric: "temp_bytes_per_hour", direction: "down",
		quantities: map[string]float64{"temp_bytes_per_hour": temp, "current_bytes": wm,
			"proposed_bytes": proposed}})
}

func (b *builder) maxWALSize() {
	cur, ok := b.v.value("max_wal_size")
	req, okR := measure(b.v.measures.CheckpointsRequested)
	timed, okT := measure(b.v.measures.CheckpointsTimed)
	wal, okW := measure(b.v.measures.WALBytesPerHour)
	if !ok || !okR || !okT || !okW {
		if ok {
			b.cov.Note("max_wal_size not judged: the checkpoint counts or the WAL rate " +
				"are unknown")
		}
		return
	}
	if req <= timed {
		return
	}
	timeout, ok := b.v.value("checkpoint_timeout")
	if !ok || timeout <= 0 {
		timeout = 300_000
	}
	cct, ok := b.v.value("checkpoint_completion_target")
	if !ok || cct < 0 || cct > 1 {
		cct = 0.9
	}
	need := wal * timeout / 3_600_000 * (1 + cct)
	value, _ := Clamp("max_wal_size", math.Ceil(need/gibibyte)*gibibyte)
	if !(parsed("max_wal_size", value) > cur) {
		b.cov.Note("max_wal_size not raised: %s already holds one checkpoint_timeout of "+
			"WAL, or is at its maximum", renderBytes(cur))
		return
	}
	b.emit(finding{kind: KindMaxWALSize, setting: "max_wal_size", value: value,
		evidence: b.v.cite("max_wal_size", "checkpoint_timeout",
			"checkpoint_completion_target"),
		reason: fmt.Sprintf("%.0f checkpoints were requested (WAL reached max_wal_size) "+
			"against %.0f timed; at %s of WAL an hour, %s holds one checkpoint_timeout.",
			req, timed, renderBytes(math.Floor(wal)), value),
		metric: "checkpoints_requested", direction: "down",
		quantities: map[string]float64{"checkpoints_requested": req,
			"checkpoints_timed": timed, "wal_bytes_per_hour": wal, "current_bytes": cur,
			"proposed_bytes": parsed("max_wal_size", value)}})
}

func (b *builder) randomPageCost() {
	rpc, ok := b.v.value("random_page_cost")
	if !ok || rpc < slowStorageCost {
		return
	}
	if !provider.IsManaged(b.v.platform) {
		b.cov.Note("random_page_cost not judged: the storage class of a server on "+
			"platform %q is not known", b.v.platform)
		return
	}
	value, _ := Clamp("random_page_cost", ssdPageCost)
	b.emit(finding{kind: KindRandomPageCost, setting: "random_page_cost", value: value,
		evidence: b.v.cite("random_page_cost"),
		reason: fmt.Sprintf("random_page_cost is %g, which prices random reads like a "+
			"spinning disk; %s storage is SSD, where %s is the usual value.", rpc,
			b.v.platform, value),
		metric: "exec_ms_per_call", direction: "down"})
}

// pg18IO: on PostgreSQL 18 an effective_io_concurrency of 1 or less set
// outside the defaults is a carried-over value (18's default is 16), and
// io_method = sync switches asynchronous I/O off (advice only: io_method
// is never proposed as SQL).
func (b *builder) pg18IO() {
	if b.v.major < 18 {
		return
	}
	s, ok := b.v.settings["effective_io_concurrency"]
	n, parsedOK := b.v.value("effective_io_concurrency")
	if ok && parsedOK && s.Source != "default" && s.Source != "" && n >= 0 && n <= 1 {
		value, _ := Clamp("effective_io_concurrency", pg18IOConcurr)
		b.emit(finding{kind: KindIOConcurrency, setting: "effective_io_concurrency",
			value: value, evidence: b.v.cite("effective_io_concurrency"),
			reason: fmt.Sprintf("effective_io_concurrency = %s (source: %s) is carried over "+
				"from before PostgreSQL 18 and switches off most read-ahead; 18's default "+
				"is %s.", s.Value, s.Source, value),
			metric: "shared_blks_read", direction: "down"})
	}
	m, ok := b.v.settings["io_method"]
	if ok && strings.EqualFold(strings.TrimSpace(m.Value), "sync") && m.Source != "default" {
		b.emit(finding{kind: KindIOMethod, setting: "io_method", value: "worker",
			evidence: b.v.cite("io_method"), advice: true,
			reason: fmt.Sprintf("io_method = sync (source: %s) switches PostgreSQL 18's "+
				"asynchronous I/O off. Unless chosen on purpose, remove it where it is set "+
				"so the default, worker, applies; the change needs a restart.", m.Source),
			metric: "shared_blks_read", direction: "down"})
	}
}

// idleInTransaction: the timeout off while sessions sit idle inside a
// transaction. It is set per database with approval (advisors-07), and
// the pack does not name the server's database, so it is advice.
func (b *builder) idleInTransaction() {
	const name = "idle_in_transaction_session_timeout"
	ms, ok := b.v.value(name)
	if !ok || ms != 0 {
		return
	}
	busiest, mean := -1, 0.0
	for _, h := range b.v.conns {
		states, _ := h.States.(map[string]any)
		n, _ := states["idle in transaction"].(float64)
		if n > mean {
			busiest, mean = h.Hour, n
		}
	}
	if busiest < 0 {
		return
	}
	value, _ := Clamp(name, idleTimeoutMs)
	b.emit(finding{kind: KindIdleInTransaction, setting: name, value: value, advice: true,
		evidence: append(b.v.cite(name), fmt.Sprintf("connections/hour/%d", busiest)),
		reason: fmt.Sprintf("%s is off while a mean %.1f sessions sit idle inside a "+
			"transaction in hour %d of the week, holding locks and snapshots. Recommended, "+
			"with an operator's approval: ALTER DATABASE <this database> SET %s = '%s' "+
			"(the upper end of the documented 5-30min; the pack has no per-session idle "+
			"times to size it closer).", name, mean, busiest, name, value),
		metric: "idle_in_transaction_sessions", direction: "down"})
}
