package main

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// evid-03a: whether each database has a confirmed owner brief and the
// accepted-step rate before and after it, read by the database's metrics
// refresher (bounded, read-only), never by the scrape.

// metricsSourceFacts is the refresh source of the owner brief gauges.
const metricsSourceFacts = "facts"

// briefFallbackSplit is where a database without a brief splits its two
// periods: the last 30 days against the 30 before.
const briefFallbackSplit = 30 * 24 * time.Hour

// ownerBriefMetricsSource reads the owner brief measure.
func ownerBriefMetricsSource(p *pgxpool.Pool) metricsSource {
	return metricsSource{name: metricsSourceFacts,
		read: func(ctx context.Context, s *metricsSnapshot) error {
			if p == nil {
				return errNoMetricsPool
			}
			m, err := facts.BriefOutcome(ctx, p, time.Now().Add(-briefFallbackSplit))
			if err != nil {
				return err
			}
			s.brief = m
			return nil
		}}
}

// writeOwnerBriefMetrics renders, for each database whose last facts
// refresh succeeded, its brief's presence and step counts.
func writeOwnerBriefMetrics(b *strings.Builder, views []metricsView) {
	metricsreg.Header(b, "pg_sage_owner_brief_present")
	for _, v := range views {
		if !v.ok(metricsSourceFacts) {
			continue
		}
		present := 0
		if v.snap.brief.Brief == facts.BriefPresent {
			present = 1
		}
		writeSample(b, "pg_sage_owner_brief_present", dbLabel(v.name), present)
	}
	b.WriteString("\n")
	metricsreg.Header(b, "pg_sage_owner_brief_steps")
	for _, v := range views {
		if !v.ok(metricsSourceFacts) {
			continue
		}
		m := v.snap.brief
		for _, p := range []struct {
			name   string
			counts facts.StepCounts
		}{{"before", m.Before}, {"after", m.After}} {
			labels := withLabel(dbLabel(v.name), "period", p.name)
			writeSample(b, "pg_sage_owner_brief_steps",
				withLabel(labels, "outcome", "accepted"), p.counts.Accepted)
			writeSample(b, "pg_sage_owner_brief_steps",
				withLabel(labels, "outcome", "rejected"), p.counts.Rejected)
		}
	}
	b.WriteString("\n")
}
