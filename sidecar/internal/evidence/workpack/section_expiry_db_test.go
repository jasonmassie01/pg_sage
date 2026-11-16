package workpack

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// evid-02b review: a fact still marked confirmed whose expires_at has
// passed is judged at Request.Now, not by its status: at Now it is gone
// (expires_at > Now keeps it), one second later it is still there.

func TestSectionsDB_ConfirmedBriefExpiresAtRequestNow(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMonitored)
	clearPacks(t, p)
	seedFixture(t, p.Monitored)
	seedCatalog(t, p)
	ctx := context.Background()
	req := sectionRequest()
	for _, tc := range []struct {
		expires time.Time
		present bool
	}{{req.Now.Add(time.Second), true}, {req.Now, false}} {
		if _, err := p.Monitored.Exec(ctx, `UPDATE sage.facts SET expires_at = $1
			WHERE fact_type = 'owner_brief' AND status = 'confirmed'`,
			tc.expires); err != nil {
			t.Fatal(err)
		}
		pack, err := Build(ctx, p.Monitored, req)
		if err != nil {
			t.Fatalf("expires %s: build: %v", tc.expires, err)
		}
		var c Context
		decodeSection(t, pack, fileContext, &c)
		b, _ := c.Brief.(map[string]any)
		gotPresent := b["brief"] != valueAbsent && b["confirmed_by"] == "ops@example.com"
		if gotPresent != tc.present {
			t.Fatalf("expires %s (now %s): brief %v, want present %v", tc.expires,
				req.Now, c.Brief, tc.present)
		}
		if tc.present && b["expires_at"] != tc.expires.UTC().Format(time.RFC3339) {
			t.Fatalf("expires_at %v, want %s", b["expires_at"], tc.expires)
		}
	}
}
