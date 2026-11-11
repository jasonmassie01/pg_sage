package briefing

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// RollupReport is one fleet roll-up: the databases whose briefings the
// message covered, in name order, and the message's delivery.
type RollupReport struct {
	Covered  []string
	Delivery map[string]Delivery
}

type rollupPart struct {
	database string
	text     string
	worker   *Worker
	id       int64
}

// RunRollup is one fleet roll-up tick (llm-06): every due database
// generates and stores its own briefing, one message covering all of them
// goes to the channels, and its delivery is written to every row it covered.
// The first due worker's channels are used; the briefing owner gives every
// worker the same settings.
func RunRollup(ctx context.Context, workers []*Worker, now time.Time) RollupReport {
	due := make([]*Worker, 0, len(workers))
	for _, w := range workers {
		if w != nil && w.ShouldRun(now) {
			due = append(due, w)
		}
	}
	sort.SliceStable(due, func(i, j int) bool { return due[i].database < due[j].database })
	parts := make([]rollupPart, 0, len(due))
	for _, w := range due {
		r, err := w.GenerateBriefing(ctx)
		if err != nil {
			w.logFn("WARN", "briefing: [%s] generation failed: %v", w.database, err)
			continue
		}
		parts = append(parts, rollupPart{database: w.database, text: r.Text,
			worker: w, id: r.ID})
	}
	report := RollupReport{Covered: make([]string, 0, len(parts))}
	if len(parts) == 0 {
		return report
	}
	report.Delivery = parts[0].worker.Dispatch(ctx, rollupText(parts))
	for _, p := range parts {
		p.worker.recordDelivery(ctx, p.id, report.Delivery)
		p.worker.MarkRan()
		report.Covered = append(report.Covered, p.database)
	}
	return report
}

// rollupText puts each database's briefing under its own heading.
func rollupText(parts []rollupPart) string {
	var b strings.Builder
	noun := "databases"
	if len(parts) == 1 {
		noun = "database"
	}
	fmt.Fprintf(&b, "# pg_sage fleet briefing: %d %s\n", len(parts), noun)
	for _, p := range parts {
		name := p.database
		if name == "" {
			name = "(unnamed database)"
		}
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", name, strings.TrimSpace(p.text))
	}
	return b.String()
}
