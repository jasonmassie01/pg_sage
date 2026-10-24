package receipt

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// CHECK-RCP-09: with history.store meta the receipts read query_store
// through histstore.Resolve, and decide exactly what they decide with the
// history in the monitored database, on the same seeded samples. The meta
// database also holds another database's samples for the same queryids
// (histfixture.Noise): a read that forgot its database scope would differ.

type historyOutcome struct {
	clean, regressed string
	targets          []changereview.ReceiptTarget
}

func TestReceiptsReadHistoryThroughTheStore(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	got := map[histstore.Mode]historyOutcome{}
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		f := newFixtureOn(t, ctx, p.Monitored, "monitored")
		f.exec(`TRUNCATE sage.change_review`)
		clean, at := f.landedReview(changereview.VerdictSafe, 901)
		regressed, at2 := f.landedReview(changereview.VerdictSafe, 902)
		p.Noise(t, at.Add(2*time.Hour), 901, 902)
		f.beforeAfter(901, at, at, 10, 5, 5)
		f.beforeAfter(902, at2, at2, 10, 5, 9)
		if rep := f.run(); rep.Decided != 2 {
			t.Fatalf("%s: decided %d, want 2 (%v)", mode, rep.Decided, rep.Errors)
		}
		c, r := f.get(clean.ID), f.get(regressed.ID)
		got[mode] = historyOutcome{clean: c.ReceiptVerdict, regressed: r.ReceiptVerdict,
			targets: append(c.Receipt.Targets, r.Receipt.Targets...)}
	}
	mon, meta := got[histstore.ModeMonitored], got[histstore.ModeMeta]
	if mon.clean != changereview.ReceiptClean || mon.regressed != changereview.ReceiptRegressed {
		t.Fatalf("monitored mode decided %s / %s, want clean / regressed", mon.clean,
			mon.regressed)
	}
	if !reflect.DeepEqual(mon, meta) {
		t.Fatalf("receipts differ by history placement:\nmonitored %+v\nmeta      %+v",
			mon, meta)
	}
}
