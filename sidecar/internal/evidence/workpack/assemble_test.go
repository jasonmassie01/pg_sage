package workpack

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/querystore"
)

// evid-02a: assembling a pack from what the history store returned. The
// same history always gives the same content hash; truncation, trend and
// size bounds are stated, never silent.
// No DB test here (build_db_test.go reads a real server); no concurrency
// test: assemble shares no state.

// synthHistory is four weeks of hourly history from monday: queryid 1
// (orders) 10 calls / 20 ms every hour, queryid 2 (customers join) 1 call /
// 100 ms every hour, and every hour covered.
func synthHistory(w Window) history {
	h := history{texts: map[int64]string{
		1: "SELECT * FROM orders WHERE id = $1",
		2: "SELECT * FROM customers c JOIN orders o ON o.c = c.id",
	}, known: knownTables, coverage: querystore.CoverageReport{Status: "ok"},
		dbSize: map[string]int64{}, tables: map[string]map[string]int64{}}
	for t := w.From; t.Before(w.To); t = t.Add(time.Hour) {
		h.covered = append(h.covered, t)
	}
	for _, q := range []struct {
		qid   int64
		calls int64
		exec  float64
	}{{1, 10, 20}, {2, 1, 100}} {
		for t := w.From; t.Before(w.To); t = t.Add(time.Hour) {
			h.hourly = append(h.hourly, hourRow{qid: q.qid, hour: t, calls: q.calls,
				execMs: q.exec, rows: q.calls})
		}
	}
	return h
}

func synthRequest(w Window) Request {
	return Request{Database: "orders_db", Window: w, Location: time.UTC,
		PGVersion: "17.2", Platform: "self-managed", HourlyDays: 35, Now: w.To}
}

func fourWeeks() Window { return Window{monday, monday.Add(28 * 24 * time.Hour)} }

func mustAssemble(t *testing.T, req Request, h history) *Pack {
	t.Helper()
	p, err := assemble(req, planDays(req.Window, req.HourlyDays, req.Now), h)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	return p
}

func workloadOf(t *testing.T, p *Pack) Workload {
	t.Helper()
	var w Workload
	if err := json.Unmarshal(p.File(fileWorkload), &w); err != nil {
		t.Fatalf("workload.json: %v", err)
	}
	return w
}

func TestAssemble_SameHistoryGivesOneHashTwentyTimes(t *testing.T) {
	w := fourWeeks()
	first := mustAssemble(t, synthRequest(w), synthHistory(w))
	for i := range 20 {
		req := synthRequest(w)
		req.Now = w.To.Add(time.Duration(i) * time.Minute) // build time is not content
		p := mustAssemble(t, req, synthHistory(w))
		if p.ContentSHA256 != first.ContentSHA256 {
			t.Fatalf("build %d: hash %s, want %s", i, p.ContentSHA256, first.ContentSHA256)
		}
	}
	if len(first.ContentSHA256) != 64 || first.Format != Format {
		t.Fatalf("pack %s %s", first.Format, first.ContentSHA256)
	}
}

func TestAssemble_ANewHourChangesTheHash(t *testing.T) {
	w := fourWeeks()
	base := mustAssemble(t, synthRequest(w), synthHistory(w))
	h := synthHistory(w)
	h.hourly[5].calls++
	if p := mustAssemble(t, synthRequest(w), h); p.ContentSHA256 == base.ContentSHA256 {
		t.Fatal("one more call in one hour left the hash unchanged")
	}
}

func TestAssemble_FamiliesTotalsAndShape(t *testing.T) {
	w := fourWeeks()
	wl := workloadOf(t, mustAssemble(t, synthRequest(w), synthHistory(w)))
	if len(wl.Families) != 2 || wl.Truncated || wl.FamiliesTotal != 2 ||
		wl.QueryIDsTotal != 2 {
		t.Fatalf("workload %+v, want two families, not truncated", wl)
	}
	join, orders := wl.Families[0], wl.Families[1]
	if join.Key != "select public.customers,public.orders" || join.ExecMs != 67200 ||
		join.Calls != 672 || orders.Key != "select public.orders" || orders.Calls != 6720 ||
		orders.ExecMs != 13440 {
		t.Fatalf("families %+v / %+v, want the join first (67200 ms) then orders", join,
			orders)
	}
	s := orders.Shape
	if s.Weeks != 4 || len(s.CallsMean) != 168 || s.CallsMean[9] != 10 ||
		s.ExecMsMean[9] != 20 || s.BucketWeeks[0] != 4 || len(s.Learning) != 0 {
		t.Fatalf("orders shape weeks %d, bucket 9 %v/%v, weeks %v, learning %v", s.Weeks,
			s.CallsMean[9], s.ExecMsMean[9], s.BucketWeeks[0], s.Learning)
	}
	if len(orders.QueryIDs) != 1 || orders.QueryIDs[0].QueryID != "1" ||
		orders.QueryIDs[0].ID != "workload/query/1" || orders.QueryIDsTotal != 1 {
		t.Fatalf("orders queryids %+v, want the one queryid", orders.QueryIDs)
	}
}

func TestAssemble_TwoWeeksMarkBucketsLearning(t *testing.T) {
	w := Window{monday, monday.Add(14 * 24 * time.Hour)}
	wl := workloadOf(t, mustAssemble(t, synthRequest(w), synthHistory(w)))
	s := wl.Families[0].Shape
	if s.Weeks != 2 || len(s.Learning) != 168 || s.BucketWeeks[100] != 2 {
		t.Fatalf("two-week shape weeks %d learning %d, want 2 and all 168", s.Weeks,
			len(s.Learning))
	}
}

func manyFamilies(w Window, n int) history {
	h := synthHistory(w)
	h.hourly, h.texts = nil, map[int64]string{}
	for q := range n {
		qid := int64(q + 1)
		h.known[fmt.Sprintf("t%03d", q)] = []string{"public"}
		h.texts[qid] = fmt.Sprintf("SELECT * FROM t%03d", q)
		h.hourly = append(h.hourly, hourRow{qid: qid, hour: w.From, calls: 1,
			execMs: float64(1000 - q)})
	}
	return h
}

func TestAssemble_FiftiethFamilyKeptFiftyFirstTruncates(t *testing.T) {
	w := fourWeeks()
	wl := workloadOf(t, mustAssemble(t, synthRequest(w), manyFamilies(w, 50)))
	if len(wl.Families) != 50 || wl.Truncated {
		t.Fatalf("50 families: kept %d truncated %v, want 50 and false",
			len(wl.Families), wl.Truncated)
	}
	wl = workloadOf(t, mustAssemble(t, synthRequest(w), manyFamilies(w, 51)))
	if len(wl.Families) != 50 || !wl.Truncated || wl.FamiliesTotal != 51 ||
		wl.Families[49].Key != "select public.t049" {
		t.Fatalf("51 families: kept %d truncated %v total %d last %q", len(wl.Families),
			wl.Truncated, wl.FamiliesTotal, wl.Families[49].Key)
	}
}

func TestAssemble_FiveHundredQueryIDsAtMost(t *testing.T) {
	w := fourWeeks()
	h := synthHistory(w)
	h.hourly, h.texts = nil, map[int64]string{}
	for q := range 30 * 30 { // 30 families of 30 queryids
		qid := int64(q + 1)
		h.known[fmt.Sprintf("t%03d", q%30)] = []string{"public"}
		h.texts[qid] = fmt.Sprintf("SELECT %d FROM t%03d", q, q%30)
		h.hourly = append(h.hourly, hourRow{qid: qid, hour: w.From, calls: 1, execMs: 1})
	}
	wl := workloadOf(t, mustAssemble(t, synthRequest(w), h))
	listed := 0
	for _, fam := range wl.Families {
		if len(fam.QueryIDs) > TopQueryIDsPerFamily || fam.QueryIDsTotal != 30 {
			t.Fatalf("family %s lists %d of %d", fam.Key, len(fam.QueryIDs), fam.QueryIDsTotal)
		}
		listed += len(fam.QueryIDs)
	}
	if listed != 500 || !wl.Truncated || wl.QueryIDsTotal != 900 {
		t.Fatalf("listed %d of %d (truncated %v), want 500 of 900", listed,
			wl.QueryIDsTotal, wl.Truncated)
	}
}

func TestAssemble_EmptyHistoryIsUnknownWithAValidHash(t *testing.T) {
	w := fourWeeks()
	empty := history{coverage: querystore.CoverageReport{Status: "unknown"}}
	p := mustAssemble(t, synthRequest(w), empty)
	wl := workloadOf(t, p)
	if wl.Families == nil || len(wl.Families) != 0 || wl.Truncated {
		t.Fatalf("empty workload %+v, want an empty (not null) family list", wl)
	}
	var cov Coverage
	if err := json.Unmarshal(p.File(fileCoverage), &cov); err != nil || cov.Status != "unknown" {
		t.Fatalf("coverage %s (%v), want unknown", p.File(fileCoverage), err)
	}
	if len(p.ContentSHA256) != 64 {
		t.Fatalf("hash %q", p.ContentSHA256)
	}
	if _, err := Resolve(p, "coverage/window/all"); err != nil {
		t.Fatalf("coverage id: %v", err)
	}
}

func TestAssemble_SingleQueryidAndNoRelationFamilies(t *testing.T) {
	w := fourWeeks()
	h := synthHistory(w)
	h.texts = map[int64]string{1: "SELECT now()", 2: ""}
	wl := workloadOf(t, mustAssemble(t, synthRequest(w), h))
	keys := map[string]int{}
	for _, fam := range wl.Families {
		keys[fam.Key] = len(fam.QueryIDs)
	}
	if keys["select"] != 1 || keys["unparsed:2"] != 1 || len(keys) != 2 {
		t.Fatalf("families %v, want select and unparsed:2 with one queryid each", keys)
	}
}

func TestAssemble_GrowthTrendNeedsThreeWeeks(t *testing.T) {
	for weeks, want := range map[int]string{2: trendUnknown, 3: trendFlat} {
		w := Window{monday, monday.Add(time.Duration(weeks) * 7 * 24 * time.Hour)}
		var g Growth
		p := mustAssemble(t, synthRequest(w), synthHistory(w))
		if err := json.Unmarshal(p.File(fileGrowth), &g); err != nil {
			t.Fatal(err)
		}
		if len(g.Workload) != 2 || g.Workload[0].Calls.Trend != want ||
			len(g.Weeks) != weeks {
			t.Fatalf("%d weeks: growth %+v, want trend %s", weeks, g.Workload, want)
		}
		if want == trendFlat && (g.Workload[0].Calls.Slope == nil ||
			*g.Workload[0].Calls.Slope != 0) {
			t.Fatalf("3 weeks of the same calls: slope %v, want 0",
				g.Workload[0].Calls.Slope)
		}
	}
}

func TestAssemble_CapacityGrowthWithStatedMissingDays(t *testing.T) {
	w := fourWeeks()
	h := synthHistory(w)
	for d := range 28 {
		if d == 3 {
			continue // no system snapshot that day
		}
		day := w.From.AddDate(0, 0, d).Format(time.DateOnly)
		h.dbSize[day] = int64(1_000_000 + d*1000)
		h.tables["public.orders"] = mapWith(h.tables["public.orders"], day, int64(d*10))
	}
	h.largest = []string{"public.orders"}
	var g Growth
	if err := json.Unmarshal(mustAssemble(t, synthRequest(w), h).File(fileGrowth), &g); err != nil {
		t.Fatal(err)
	}
	if g.DBSize.ID != "growth/db/size" || g.DBSize.MissingDays != 1 ||
		g.DBSize.Fit.Slope == nil || *g.DBSize.Fit.Slope != 1000 || g.DBSize.Values[3] != nil ||
		g.DBSize.Fit.Trend != trendRising || g.DBSize.Unit != "bytes" {
		t.Fatalf("db size %+v, want 1000 bytes/day with day 4 missing", g.DBSize)
	}
	if len(g.Tables) != 1 || g.Tables[0].ID != "growth/table/public.orders" ||
		*g.Tables[0].Fit.Slope != 10 {
		t.Fatalf("tables %+v, want public.orders at 10 bytes/day", g.Tables)
	}
}

func mapWith(m map[string]int64, k string, v int64) map[string]int64 {
	if m == nil {
		m = map[string]int64{}
	}
	m[k] = v
	return m
}

func TestAssemble_TooLargeNamesTheLargestSection(t *testing.T) {
	w := fourWeeks()
	req := synthRequest(w)
	req.MaxBodyBytes = 512
	_, err := assemble(req, planDays(w, 35, w.To), synthHistory(w))
	if !errors.Is(err, ErrPackTooLarge) || !strings.Contains(err.Error(), fileWorkload) {
		t.Fatalf("assemble over the bound: %v, want ErrPackTooLarge naming %s", err,
			fileWorkload)
	}
}

func TestAssemble_DailyDaysFeedTotalsNotTheShape(t *testing.T) {
	to := monday.Add(60 * 24 * time.Hour)
	w := Window{monday, to}
	h := synthHistory(Window{monday.Add(25 * 24 * time.Hour), to})
	h.daily = []dayRow{{qid: 1, day: monday, calls: 1000, execMs: 5}}
	p := mustAssemble(t, Request{Database: "d", Window: w, Location: time.UTC,
		HourlyDays: 35, Now: to}, h)
	wl := workloadOf(t, p)
	var orders Family
	for _, fam := range wl.Families {
		if fam.Key == "select public.orders" {
			orders = fam
		}
	}
	if orders.Calls != 1000+10*35*24 || orders.Shape.Weeks != 5 {
		t.Fatalf("orders calls %d weeks %d, want the daily 1000 in the total and a "+
			"5-week hourly shape", orders.Calls, orders.Shape.Weeks)
	}
	var hdr Header
	if err := json.Unmarshal(p.File(fileHeader), &hdr); err != nil || hdr.Days[0].Granularity !=
		granularityDaily || hdr.Days[30].Granularity != granularityHourly {
		t.Fatalf("header days %v (%v)", hdr.Days[:2], err)
	}
}

func TestResolve_EveryListedIDResolves(t *testing.T) {
	w := fourWeeks()
	p := mustAssemble(t, synthRequest(w), synthHistory(w))
	ids := listedIDs(t, p)
	if len(ids) < 7 {
		t.Fatalf("ids %v, want the coverage, 2 families, 2 queries, 2 family growths, "+
			"db size", ids)
	}
	for _, id := range ids {
		fact, err := Resolve(p, id)
		if err != nil || fact.ID != id || len(fact.Value) == 0 || fact.Source == "" ||
			!fact.Window.From.Equal(w.From) {
			t.Fatalf("Resolve(%s) = %+v, %v", id, fact, err)
		}
	}
	if got := p.IDs(); len(got) != len(ids) {
		t.Fatalf("IDs() lists %d, the files hold %d", len(got), len(ids))
	}
}

// listedIDs walks every section file and returns each "id" value.
func listedIDs(t *testing.T, p *Pack) []string {
	t.Helper()
	seen := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if s, ok := e.(string); ok && k == "id" {
					seen[s] = true
				}
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	for _, f := range p.Files {
		var v any
		if err := json.Unmarshal(f.Data, &v); err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		walk(v)
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out
}

func TestResolve_UnknownIDs(t *testing.T) {
	w := fourWeeks()
	p := mustAssemble(t, synthRequest(w), synthHistory(w))
	for _, id := range []string{"", "workload", "workload/family/", "plans/x/y",
		"workload/family/../coverage", "coverage/window/all/..", "workload/query/999",
		"workload/nope/1", "growth/table/public.missing", "coverage//all"} {
		if _, err := Resolve(p, id); !errors.Is(err, ErrUnknownEvidence) {
			t.Errorf("Resolve(%q): %v, want ErrUnknownEvidence", id, err)
		}
	}
	if _, err := Resolve(nil, "coverage/window/all"); !errors.Is(err, ErrUnknownEvidence) {
		t.Fatalf("Resolve on no pack: %v", err)
	}
}

func TestDecode_RoundTripsAndRefusesATamperedBody(t *testing.T) {
	w := fourWeeks()
	p := mustAssemble(t, synthRequest(w), synthHistory(w))
	back, err := Decode(p.Body, p.Manifest)
	if err != nil || back.ContentSHA256 != p.ContentSHA256 || len(back.IDs()) != len(p.IDs()) {
		t.Fatalf("decode: %v (%s), want the same pack", err, back.ContentSHA256)
	}
	m := p.Manifest
	m.ContentSHA256 = strings.Repeat("0", 64)
	if _, err := Decode(p.Body, m); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("decode against another hash: %v, want ErrCorrupt", err)
	}
	if _, err := Decode([]byte("not gzip"), p.Manifest); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("decode of garbage: %v, want ErrCorrupt", err)
	}
}
