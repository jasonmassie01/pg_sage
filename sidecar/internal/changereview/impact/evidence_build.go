package impact

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

// statementEvidence is "stmt:<n>" for s.
func (r *run) statementEvidence(s *stmt) {
	d := stmtData{Index: s.index, File: s.file, Line: s.line, Kind: s.kind, SQL: s.sql,
		Relations: s.relations, ParseMode: "ast"}
	if s.text {
		d.ParseMode = "text"
	}
	for _, p := range s.parts {
		if m := statementLock(p); m != "" && d.LockMode == "" {
			d.LockMode = m
		}
		d.InTransactionBlock = d.InTransactionBlock || p.InTransactionBlock
		if p.LockTimeoutMs != nil {
			d.LockTimeoutMs = p.LockTimeoutMs
		}
	}
	label := fmt.Sprintf("statement %d (%s)", s.index, s.kind)
	if s.file != "" {
		label += " " + s.file + ":" + strconv.Itoa(s.line)
	}
	kind, ref, _ := strings.Cut(s.evID, ":")
	s.evID = r.addEvidence(kind, ref, label, d)
}

// tableEvidence is "table:<schema.name>" for every relation found.
func (r *run) tableEvidence() {
	for _, rel := range r.rels {
		if !rel.found {
			continue
		}
		d := tableData{Relation: rel.name, Kind: relkindNames[rel.relkind],
			RelTuples: rel.reltuples, TotalBytes: rel.totalBytes, HeapBytes: rel.heapBytes,
			Publications: rel.publications, RateKnown: rel.rate.known}
		if rel.relkind == "p" {
			d.Partitions = rel.inherits
		} else {
			d.Children = rel.inherits
		}
		if rel.rate.known {
			d.InsPerS, d.UpdPerS, d.DelPerS = rel.rate.ins, rel.rate.upd, rel.rate.del
			d.RateSince = rel.rate.since.UTC().Format(time.RFC3339)
		}
		rel.evID = r.addEvidence("table", rel.name, fmt.Sprintf("%s: %.0f rows, %d bytes",
			rel.name, rel.reltuples, rel.totalBytes), d)
	}
}

// queryEvidence is "queryid:<id>" for ws, with the columns it reads of rel.
func (r *run) queryEvidence(ws *workStmt, rel *relation) string {
	if ws.evID != "" {
		return ws.evID
	}
	d := queryData{QueryID: ws.id, Query: ws.text, Calls: ws.calls,
		RateKnown: ws.rateKnown, LockMode: string(ws.lockMode()), Kind: string(ws.refs.Kind),
		Columns: columnsOf(ws.refs, rel), Star: readsStar(ws.refs, rel)}
	if ws.rateKnown {
		d.Calls7d, d.PeakPerS = ws.rate.calls, ws.rate.peak
	}
	if len(ws.tags) > 0 {
		d.Tags = ws.tags
	}
	ws.evID = r.addEvidence("queryid", strconv.FormatInt(ws.id, 10),
		fmt.Sprintf("queryid %d: %d calls", ws.id, ws.calls), d)
	return ws.evID
}

// refersTo reports whether ref names rel (a schema-less ref matches by
// name, as the workload index filed it).
func refersTo(ref sqlast.QualifiedName, rel *relation) bool {
	return ref.Name == rel.rel && (ref.Schema == "" || ref.Schema == rel.schema)
}

// columnsOf is the columns of rel a statement reads or writes, sorted.
func columnsOf(refs sqlast.Refs, rel *relation) []string {
	var out []string
	for _, c := range refs.Columns {
		if refersTo(c.Relation, rel) && !hasString(out, c.Column) {
			out = append(out, c.Column)
		}
	}
	sort.Strings(out)
	return out
}

func readsStar(refs sqlast.Refs, rel *relation) bool {
	for _, n := range refs.Star {
		if refersTo(n, rel) {
			return true
		}
	}
	return false
}

// ambiguousOn is whether col appears unqualified over a join that
// includes rel, so the statement may read rel's col.
func ambiguousOn(refs sqlast.Refs, rel *relation, col string) bool {
	for _, a := range refs.Ambiguous {
		if a.Column != col {
			continue
		}
		for _, c := range a.Candidates {
			if refersTo(c, rel) {
				return true
			}
		}
	}
	return false
}
