package earned

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

// The level of index_replace is the lowest of its own row (when it has
// one) and its components' levels: a database that hands both creates and
// drops to a human gets replace cards; demoting either component demotes
// the replace with it.

func (f *fixture) seedLevel(family Family, class ActionClass, level Level) {
	f.t.Helper()
	err := f.store.withTx(f.ctx, func(tx pgx.Tx) error {
		_, _, err := f.store.seedGrandfathered(f.ctx, tx, family, class, level,
			"test seed", f.clock.Now())
		return err
	})
	if err != nil {
		f.t.Fatalf("seed %s/%s: %v", family, class, err)
	}
}

func TestIndexReplaceLevelFollowsItsComponents(t *testing.T) {
	cases := []struct {
		name               string
		create, drop, own  Level
		hasOwn             bool
		granted, effective Level
	}{
		{"both components at L2", L2, L2, 0, false, L2, L2},
		{"both at L3: granted L3, capped at L2", L3, L3, 0, false, L3, L2},
		{"drop still at its default", L3, 0, 0, false, L1, L1},
		{"own row demoted", L3, L3, L1, true, L1, L1},
		{"own row above a component", L2, L2, L3, true, L2, L2},
	}
	for _, c := range cases {
		lf := newLimiterFixture(t)
		if c.create > 0 {
			lf.seedLevel(FamilyTuning, ClassIndexCreate, c.create)
		}
		if c.drop > 0 {
			lf.seedLevel(FamilyHygiene, ClassIndexDrop, c.drop)
		}
		if c.hasOwn {
			lf.seedLevel(FamilyTuning, ClassIndexReplace, c.own)
		}
		if got := lf.granted(FamilyTuning, ClassIndexReplace); got != c.granted {
			t.Fatalf("%s: granted = %v, want %v", c.name, got, c.granted)
		}
		limit, err := lf.lim.Limit(lf.ctx, selfReq("replace_index",
			"CREATE INDEX CONCURRENTLY i ON public.orders (a, b)"))
		if err != nil || Level(limit.Level) != c.effective ||
			limit.Class != string(ClassIndexReplace) || limit.Family != string(FamilyTuning) {
			t.Fatalf("%s: limit = %+v, %v; want level %v", c.name, limit, err, c.effective)
		}
	}
}
