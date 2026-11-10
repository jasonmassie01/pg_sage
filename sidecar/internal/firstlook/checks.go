package firstlook

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/rolegrants"
	"github.com/pg-sage/sidecar/internal/sqlident"
)

// indexChecks reads the indexes and foreign keys once and evaluates the
// invalid, duplicate, never-scanned and unindexed-foreign-key rules.
func (p *pass) indexChecks(ctx context.Context, tx pgx.Tx) ([]outcome, error) {
	idx, truncated, err := readIndexes(ctx, tx)
	if err != nil {
		return nil, err
	}
	fks, err := readForeignKeys(ctx, tx)
	if err != nil {
		return nil, err
	}
	keysErr := readUniqueKeys(ctx, tx, idx)
	partsErr := readUnattached(ctx, tx, idx)
	out := p.evalIndexes(idx, fks, readIndexBuilds(ctx, tx), truncated)
	if keysErr != nil { // out[0] is the invalid-index rule
		out[0].note = strings.TrimSpace(out[0].note + " No duplicate-key query for " +
			"invalid unique indexes: " + keysErr.Error())
	}
	if partsErr != nil {
		out[0].note = strings.TrimSpace(out[0].note + " No partitions named for invalid " +
			"partitioned indexes: " + partsErr.Error())
	}
	return out, nil
}

// evalIndexes evaluates the index rules; builds are the index builds in
// progress, and truncated means the index read stopped at its row limit,
// so every rule saw only part of the indexes.
func (p *pass) evalIndexes(idx []Index, fks []ForeignKey, builds IndexBuilds,
	truncated bool) []outcome {
	th := p.opts.Thresholds
	dups, flagged := DuplicateIndexes(idx)
	unscanned := idx
	if truncated {
		unscanned = withoutPartitioned(idx)
	}
	out := []outcome{
		p.add(RuleInvalidIndex, InvalidIndexes(idx, builds)),
		p.add(RuleDuplicateIndex, dups),
		p.add(RuleNeverScannedIndex, NeverScannedIndexes(unscanned, flagged, p.window,
			p.opts.Now())),
		p.add(RuleUnindexedFK, UnindexedForeignKeys(fks, idx, th.FKMinRows)),
	}
	if truncated {
		for i := range out {
			out[i].truncated = true
			out[i].note = strings.TrimSpace(fmt.Sprintf("only the first %d indexes were "+
				"read. %s", maxIndexRows, out[i].note))
		}
	}
	return out
}

func (p *pass) xid(ctx context.Context, tx pgx.Tx) ([]outcome, error) {
	x, err := readXID(ctx, tx)
	if err != nil {
		return nil, err
	}
	items := XIDRunway(x, p.opts.Thresholds)
	o := p.add(RuleXIDRunway, items)
	o.note = fmt.Sprintf("age(datfrozenxid) %d, mxid_age(datminmxid) %d",
		x.DatFrozenXIDAge, x.DatMinMXIDAge)
	return []outcome{o}, nil
}

func (p *pass) sequences(ctx context.Context, tx pgx.Tx) ([]outcome, error) {
	seqs, err := readSequences(ctx, tx)
	if err != nil {
		return nil, err
	}
	items, unreadable := SequenceRunway(seqs, p.opts.Thresholds)
	o := p.add(RuleSequenceRunway, items)
	if unreadable > 0 {
		var role string
		if err := tx.QueryRow(ctx, tag+"SELECT current_user::text").Scan(&role); err != nil {
			return nil, fmt.Errorf("read the connected role: %w", err)
		}
		o.note = degradedPrefix + unreadableNote(seqs, unreadable, role)
	}
	return []outcome{o}, nil
}

// unreadableNote names the grant that lets role read the sequences: the
// SQL the Grant more guide and the startup check print (rolegrants).
func unreadableNote(seqs []Sequence, n int, role string) string {
	schemas := map[string]bool{}
	for _, s := range seqs {
		if s.LastValue == nil {
			schemas[s.Schema] = true
		}
	}
	var names []string
	for s := range schemas {
		names = append(names, s)
	}
	sort.Strings(names)
	for i, s := range names {
		names[i] = sqlident.Ident(s)
	}
	return fmt.Sprintf("%d sequences are not readable by the pg_sage role, so their "+
		"runway is unknown: %s;", n, rolegrants.SequenceSelectSQL(names, sqlident.Role(role)))
}

func (p *pass) bloat(ctx context.Context, tx pgx.Tx) ([]outcome, error) {
	ts, err := readTableStats(ctx, tx, p.opts.Thresholds.BloatMinBytes)
	if err != nil {
		return nil, err
	}
	return []outcome{p.add(RuleTableBloat, TableBloat(ts, p.opts.Thresholds))}, nil
}

func (p *pass) testSchemas(ctx context.Context, tx pgx.Tx) ([]outcome, error) {
	ss, err := readTestSchemas(ctx, tx, facts.TestSchemaPattern())
	if err != nil {
		return nil, err
	}
	now := p.opts.Now()
	p.report.FactProposals = fixtureProposals(ss, p.window, now)
	o := p.add(RuleTestSchema, TestSchemas(ss, p.window, now))
	if n := len(p.report.FactProposals); n > 0 {
		o.note = fmt.Sprintf("%d proposed as test-fixture facts for an operator to "+
			"confirm", n)
	}
	return []outcome{o}, nil
}

func (p *pass) extensions(ctx context.Context, tx pgx.Tx) ([]outcome, error) {
	e, err := readExtensions(ctx, tx)
	if err != nil {
		return nil, err
	}
	logs, err := readLogSettings(ctx, tx)
	if err != nil {
		return nil, err
	}
	caps, items := ExtensionCapabilities(e, p.opts.Provider)
	logRCA := LogRCACapability(logs, p.opts.Provider)
	if it, ok := LogRCAItem(logRCA); ok {
		items = append(items, it)
	}
	p.report.Capabilities = append(caps, logRCA)
	return []outcome{p.add(RuleMissingExtension, items)}, nil
}
