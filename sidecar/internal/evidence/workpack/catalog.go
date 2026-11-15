package workpack

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// The catalog reads of a build (evid-02b): each section reads the
// monitored database in its own catalogread.Begin transaction (READ ONLY,
// statement and lock timeouts from the safety configuration, no parallel
// workers, no JIT). One failed section fails the build, its error naming
// the section and the bounds, so a pack is never silently partial; the
// provider section alone is optional and records "unavailable" with the
// error class instead.

// catalogData is what the catalog reads returned.
type catalogData struct {
	tables  []schemaTableRow
	indexes []indexRow
	stats   []statRow
	plans   map[int64]planRow
	facts   []factRow
	host    *hostRow
	hostErr error
}

// catalogInput is what the catalog reads need from the request and the
// history: the window, the top queryids and the referenced tables.
type catalogInput struct {
	from, to, now time.Time
	resource      string
	samples       bool
	top           []int64
	relations     []string
}

// catalogSection is one catalog read.
type catalogSection struct {
	name     string
	what     string
	optional bool
	read     func(context.Context, pgx.Tx, catalogInput, *catalogData) error
}

var catalogSections = []catalogSection{
	{name: "schema", what: "the table catalog", read: readSchema},
	{name: "indexes", what: "the index catalog", read: readIndexes},
	{name: "stats", what: "pg_stats", read: readStats},
	{name: "plans", what: "sage.explain_cache", read: readPlans},
	{name: "context", what: "sage.facts", read: readContext},
	{name: "provider", what: "sage.host_usage_hourly", optional: true, read: readProvider},
}

var errNoCatalog = errors.New("workpack: the catalog sections need the monitored " +
	"database's pool")

// readCatalog runs every catalog section on db.
func readCatalog(ctx context.Context, db any, req Request, in catalogInput) (catalogData,
	error) {
	b, ok := db.(catalogread.Beginner)
	if !ok || b == nil {
		return catalogData{}, errNoCatalog
	}
	t := req.timeouts()
	var cat catalogData
	for _, s := range catalogSections {
		err := runSection(ctx, b, t, s, in, &cat)
		if err == nil {
			continue
		}
		if s.optional {
			cat.hostErr = err
			continue
		}
		return catalogData{}, fmt.Errorf("workpack: %s section: read %s (statement_timeout "+
			"%s, lock_timeout %s): %w", s.name, s.what, t.Statement, t.Lock, err)
	}
	return cat, nil
}

func runSection(ctx context.Context, b catalogread.Beginner, t catalogread.Timeouts,
	s catalogSection, in catalogInput, cat *catalogData) error {
	tx, err := catalogread.Begin(ctx, b, t)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	return s.read(ctx, tx, in, cat)
}

// timeouts are the request's bounds, the shipped safety bounds when unset.
func (r Request) timeouts() catalogread.Timeouts {
	if r.Timeouts == (catalogread.Timeouts{}) {
		return catalogread.Default()
	}
	return r.Timeouts
}

// catalogInputOf is what the catalog reads of req need after the history.
func catalogInputOf(req Request, plan dayPlan, h history) catalogInput {
	wl, _ := buildWorkload(req, plan, h)
	return catalogInput{from: req.Window.From, to: req.Window.To, now: req.Now,
		resource: req.Resource, samples: req.IncludeValueSamples, top: topQueryIDs(wl),
		relations: referencedTables(wl)}
}
