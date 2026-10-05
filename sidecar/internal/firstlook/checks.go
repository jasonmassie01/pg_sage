package firstlook

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/facts"
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
	th := p.opts.Thresholds
	dups, flagged := DuplicateIndexes(idx)
	out := []outcome{
		p.add(RuleInvalidIndex, InvalidIndexes(idx)),
		p.add(RuleDuplicateIndex, dups),
		p.add(RuleNeverScannedIndex, NeverScannedIndexes(idx, flagged, p.window,
			p.opts.Now())),
		p.add(RuleUnindexedFK, UnindexedForeignKeys(fks, idx, th.FKMinRows)),
	}
	if truncated {
		for i := range out {
			out[i].note = strings.TrimSpace(fmt.Sprintf("only the first %d indexes were "+
				"read. %s", maxIndexRows, out[i].note))
		}
	}
	return out, nil
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
		o.note = degradedPrefix + unreadableNote(seqs, unreadable)
	}
	return []outcome{o}, nil
}

// unreadableNote names the grant that lets the role read the sequences.
func unreadableNote(seqs []Sequence, n int) string {
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
	grants := make([]string, 0, len(names))
	for i, s := range names {
		if i == 3 {
			grants = append(grants, "...")
			break
		}
		grants = append(grants, fmt.Sprintf("GRANT SELECT ON ALL SEQUENCES IN SCHEMA %s "+
			"TO <pg_sage role>;", ident(s)))
	}
	return fmt.Sprintf("%d sequences are not readable by the pg_sage role, so their "+
		"runway is unknown: %s", n, strings.Join(grants, " "))
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
	caps, items := ExtensionCapabilities(e, p.opts.Provider)
	p.report.Capabilities = caps
	return []outcome{p.add(RuleMissingExtension, items)}, nil
}
