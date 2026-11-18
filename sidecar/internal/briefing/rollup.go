package briefing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	digest   Digest
	worker   *Worker
	id       int64
}

// rollupScope is the scope the fleet roll-up digest is deduplicated in.
const rollupScope = "fleet"

// RunRollup is one fleet roll-up tick (llm-06): every due database
// generates and stores its own briefing, one digest message covering all
// of them is sent (ux-02b; unless unchanged), and its delivery is written
// to every row it covered. The first due worker's channels and notifier
// are used; the briefing owner gives every worker the same settings.
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
		parts = append(parts, rollupPart{database: w.database,
			digest: digestOf(r, w.database), worker: w, id: r.ID})
	}
	report := RollupReport{Covered: make([]string, 0, len(parts))}
	if len(parts) == 0 {
		return report
	}
	report.Delivery = parts[0].worker.deliverDigest(ctx, rollupSend(parts))
	for _, p := range parts {
		p.worker.recordDelivery(ctx, p.id, report.Delivery)
		p.worker.MarkRan()
		report.Covered = append(report.Covered, p.database)
	}
	return report
}

// rollupSend is the roll-up message: one headline over the summed counts,
// a hash over every part's, and each database's digest under its heading.
func rollupSend(parts []rollupPart) digestSend {
	var sum DigestCounts
	keys := make([]string, 0, len(parts))
	empty := true
	for _, p := range parts {
		c := p.digest.Counts
		sum.Changes += c.Changes
		sum.Verified += c.Verified
		sum.Reverted += c.Reverted
		sum.NeedsYou += c.NeedsYou
		sum.HeldBack += c.HeldBack
		keys = append(keys, p.database+"="+p.digest.Hash)
		empty = empty && p.digest.Empty()
	}
	return digestSend{scope: rollupScope, database: "all", hash: hashOf(keys),
		empty: empty, briefingID: parts[0].id, headline: DeterministicHeadline(sum),
		text: rollupText(parts)}
}

// hashOf is the hex SHA-256 of the sorted keys.
func hashOf(keys []string) string {
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:])
}

// rollupText puts each database's digest under its own heading.
func rollupText(parts []rollupPart) string {
	var b strings.Builder
	noun := "databases"
	if len(parts) == 1 {
		noun = "database"
	}
	fmt.Fprintf(&b, "# pg_sage fleet digest: %d %s\n", len(parts), noun)
	for _, p := range parts {
		name := p.database
		if name == "" {
			name = "(unnamed database)"
		}
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", name, strings.TrimSpace(p.digest.Text()))
	}
	return b.String()
}
