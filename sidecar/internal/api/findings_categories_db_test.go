package api

import (
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/findingcat"
)

// registrySeed is one stored category per registry entry: the name, or a
// member of a prefix family.
func registrySeed() []string {
	var out []string
	for _, c := range findingcat.All() {
		if c.Prefix {
			out = append(out, c.Name+"zz_seed")
			continue
		}
		out = append(out, c.Name)
	}
	return out
}

func TestEveryRegistrySubsystemFiltersExactlyItsRows(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	tag := fmt.Sprintf("catfilter%d", time.Now().UnixNano())
	ids := map[string]string{}
	for _, c := range registrySeed() {
		var id int64
		err := pool.QueryRow(ctx, `INSERT INTO sage.findings
			(category, severity, object_type, object_identifier, title, detail, status)
			VALUES ($1, 'info', 'table', $2, 'category filter seed', '{}', 'open')
			RETURNING id`, c, "public."+tag+"_"+c).Scan(&id)
		if err != nil {
			t.Fatalf("seed %s finding: %v", c, err)
		}
		ids[strconv.FormatInt(id, 10)] = c
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.findings WHERE object_identifier LIKE $1",
			"public."+tag+"_%")
	})
	h := findingsListHandler(phase2MgrWithPool(pool))
	for _, source := range findingcat.Subsystems() {
		resp, status := getList(t, h,
			"/api/v1/findings?database=testdb&limit=500&sort=created_at&source="+source)
		if status != 200 {
			t.Fatalf("source=%s status=%d", source, status)
		}
		var got []string
		for _, row := range resp.Findings {
			if c, ok := ids[fmt.Sprint(row["id"])]; ok {
				got = append(got, c)
			}
		}
		for _, c := range registrySeed() {
			entry, _ := findingcat.Lookup(c)
			want := entry.Subsystem == source || slices.Contains(entry.AlsoIn, source)
			if want != slices.Contains(got, c) {
				t.Errorf("source=%s returned %v; seeded %q (subsystem %s) want listed=%v",
					source, got, c, entry.Subsystem, want)
			}
		}
	}
}
