package tuning

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/llm"
)

// The case packet: what the model sees about a case. It is bounded, its
// statement texts are redacted, everything database-derived sits in one
// untrusted <data> block, and every statement, table and fact carries an
// evidence ID the proposals must cite.

const (
	maxPacketBytes    = 12000
	maxStatementChars = 1000
	maxPacketIndexes  = 12
)

// Evidence is one citable item: a statement (S), table (T), confirmed
// fact (F) or tool result (R).
type Evidence struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
	Text string `json:"text"`
}

// evidenceSet holds a case's citable evidence by ID.
type evidenceSet map[string]Evidence

// packet is the user message about a case and its evidence.
type packet struct {
	Text     string
	Evidence evidenceSet
}

// packetBuilder writes the data block within a byte budget.
type packetBuilder struct {
	b         strings.Builder
	limit     int
	cut       int
	evidence  evidenceSet
	truncated bool
}

// line adds a line when it fits; otherwise it counts it as cut.
func (p *packetBuilder) line(format string, args ...any) bool {
	s := fmt.Sprintf(format, args...) + "\n"
	if p.truncated || p.b.Len()+len(s) > p.limit {
		p.truncated = true
		p.cut++
		return false
	}
	p.b.WriteString(s)
	return true
}

func (p *packetBuilder) cite(ev Evidence, format string, args ...any) {
	if p.line(ev.ID+" "+format, args...) {
		p.evidence[ev.ID] = ev
	}
}

func (p *packetBuilder) text() string {
	if p.truncated {
		fmt.Fprintf(&p.b, "... truncated: %d more line(s) left out\n", p.cut)
	}
	return p.b.String()
}

// packetFor builds the packet of case c on snapshot cur.
func (a *Agent) packetFor(ctx context.Context, c Case, cur *collector.Snapshot, w Workload,
	confirmed []facts.Fact) packet {
	header := fmt.Sprintf("Case %s (%s): %s\n", c.ID, c.Kind, c.Reason)
	instructions := answerInstructions(a.allowedTypes())
	p := &packetBuilder{evidence: evidenceSet{},
		limit: maxPacketBytes - len(header) - len(instructions) - 128}
	p.line("Case %s, %s: %s", c.ID, c.Kind, c.Reason)
	writeStatements(p, c)
	a.writeTables(ctx, p, c, cur, w)
	writeFacts(p, confirmed, a.now(), c.Tables)
	writeSettings(p, cur)
	return packet{Text: header + llm.UntrustedData("tuning_case", p.text()) + "\n\n" +
		instructions, Evidence: p.evidence}
}

func writeStatements(p *packetBuilder, c Case) {
	if len(c.Statements) == 0 {
		return
	}
	p.line("Statements (interval):")
	for i, s := range c.Statements {
		ev := Evidence{ID: fmt.Sprintf("S%d", i+1), Kind: "statement",
			Ref:  fmt.Sprintf("queryid:%d", s.QueryID),
			Text: fmt.Sprintf("%d calls, %.0f ms", s.Calls, s.TotalMs)}
		p.cite(ev, "queryid %d [%s]: %d calls, %.0f ms (%.1f%% of workload time), mean "+
			"%.2f ms (before %.2f ms), rows %d, shared blocks read/hit %d/%d, temp blocks "+
			"written %d", s.QueryID, s.Class, s.Calls, s.TotalMs, s.Share*100, s.MeanMs,
			s.PrevMeanMs, s.Rows, s.SharedBlksRead, s.SharedBlksHit, s.TempBlksWritten)
		if !s.Windowed {
			p.line("   (no earlier sample: counters since it was first tracked, not a rate)")
		}
		p.line("   %s", clip(oneLine(llm.SanitizeForLLM(s.Text)), maxStatementChars))
	}
}

// writeTables describes each case table: size, writes, indexes, columns,
// storage parameters and the index shapes already measured and rejected.
func (a *Agent) writeTables(ctx context.Context, p *packetBuilder, c Case,
	cur *collector.Snapshot, w Workload) {
	if len(c.Tables) == 0 {
		return
	}
	p.line("Tables:")
	for i, name := range c.Tables {
		ts, ok := findSnapshotTable(cur, name)
		if !ok {
			continue
		}
		ev := Evidence{ID: fmt.Sprintf("T%d", i+1), Kind: "table", Ref: name,
			Text: fmt.Sprintf("%d live rows, %d dead", ts.NLiveTup, ts.NDeadTup)}
		p.cite(ev, "%s [%s]: %d live rows, %d dead, heap %d bytes, inserts %d, updates %d "+
			"(HOT %d), deletes %d, seq scans %d, index scans %d", name, w.Tables[name].Class,
			ts.NLiveTup, ts.NDeadTup, ts.TableBytes, ts.NTupIns, ts.NTupUpd, ts.NTupHotUpd,
			ts.NTupDel, ts.SeqScan, ts.IdxScan)
		writeIndexes(p, cur, ts)
		a.writeTableContext(ctx, p, cur, name)
		if ro := reloptions(cur, ts); ro != "" {
			p.line("   storage parameters: %s", ro)
		}
	}
}

func writeIndexes(p *packetBuilder, cur *collector.Snapshot, ts collector.TableStats) {
	n := 0
	for _, ix := range cur.Indexes {
		if ix.SchemaName != ts.SchemaName || ix.RelName != ts.RelName {
			continue
		}
		if n++; n > maxPacketIndexes {
			p.line("   ... more indexes left out")
			return
		}
		p.line("   index %s: %s; %d scans, %d bytes, valid %t", ix.IndexRelName,
			llm.SanitizeForLLM(ix.IndexDef), ix.IdxScan, ix.IndexBytes, ix.IsValid)
	}
}

// writeTableContext adds the columns and the measured rejections.
func (a *Agent) writeTableContext(ctx context.Context, p *packetBuilder,
	cur *collector.Snapshot, name string) {
	tc, ok := a.tableContext(ctx, cur, name)
	if !ok {
		return
	}
	cols := make([]string, 0, len(tc.Columns))
	for _, col := range tc.Columns {
		cols = append(cols, col.Name+" "+col.Type)
	}
	if len(cols) > 0 {
		p.line("   columns: %s", strings.Join(cols, ", "))
	}
	if a.deps.Indexes == nil {
		return
	}
	measured := a.deps.Indexes.MeasuredRejections(ctx, tc)
	if len(measured) > 0 {
		p.line("   index shapes already measured with HypoPG and rejected (do not " +
			"propose them again):")
		for _, m := range measured {
			p.line("   %s", m)
		}
	}
}

func writeFacts(p *packetBuilder, confirmed []facts.Fact, now time.Time, tables []string) {
	var lines []string
	var ids []int64
	for _, f := range confirmed {
		for _, l := range facts.PromptLines([]facts.Fact{f}, now, tables...) {
			lines, ids = append(lines, l), append(ids, f.ID)
		}
	}
	if len(lines) == 0 {
		return
	}
	p.line("Confirmed facts (binding; a change they bind becomes a source-fix packet):")
	for i, l := range lines {
		id := fmt.Sprintf("F%d", ids[i])
		p.cite(Evidence{ID: id, Kind: "fact", Ref: fmt.Sprintf("fact:%d", ids[i]),
			Text: l}, "%s", l)
	}
}

// packetSettings are the configuration values the agent may tune or must
// reason about.
var packetSettings = []string{"work_mem", "hash_mem_multiplier", "maintenance_work_mem",
	"shared_buffers", "effective_cache_size", "random_page_cost", "max_connections",
	"default_statistics_target", "effective_io_concurrency", "autovacuum_vacuum_scale_factor",
	"autovacuum_vacuum_threshold", "autovacuum_vacuum_cost_delay",
	"autovacuum_vacuum_cost_limit", "autovacuum_max_workers", "autovacuum_naptime"}

func writeSettings(p *packetBuilder, cur *collector.Snapshot) {
	if cur == nil || cur.ConfigData == nil {
		return
	}
	first := true
	for _, name := range packetSettings {
		for _, s := range cur.ConfigData.PGSettings {
			if s.Name != name {
				continue
			}
			if first {
				p.line("Settings:")
				first = false
			}
			p.line("   %s = %s%s (source %s, context %s)", s.Name, s.Setting, s.Unit,
				s.Source, s.Context)
		}
	}
}
