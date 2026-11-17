package workpack

import (
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/baseline"
)

// The workload section: per-queryid totals over the window (hourly rows of
// the hourly part, daily rows of the counted daily days), grouped into
// families, and each family's hour-of-week shape from the hourly part.
// Every float is summed over rows in (queryid, time) order and queryids in
// ascending order, so the same rows always give the same sums.

// qtotal is one queryid's totals.
type qtotal struct {
	qid    int64
	calls  int64
	execMs float64
	rows   int64
}

// famAcc accumulates one family.
type famAcc struct {
	key, cmd   string
	relations  []string
	calls      int64
	execMs     float64
	rows       int64
	qids       []qtotal
	bucketCall [baseline.BucketsPerWeek]float64
	bucketExec [baseline.BucketsPerWeek]float64
}

// inHourly reports an hour of the hourly part.
func inHourly(plan dayPlan, w Window, t time.Time) bool {
	return !t.Before(plan.hourlyFrom) && t.Before(w.To)
}

// queryTotals sums the window's rows per queryid, in queryid order.
func queryTotals(plan dayPlan, w Window, h history) []qtotal {
	daily := map[string]bool{}
	for _, d := range plan.dailyDays {
		daily[d] = true
	}
	byQID := map[int64]*qtotal{}
	add := func(qid, calls int64, exec float64, rows int64) {
		q := byQID[qid]
		if q == nil {
			q = &qtotal{qid: qid}
			byQID[qid] = q
		}
		q.calls, q.execMs, q.rows = q.calls+calls, q.execMs+exec, q.rows+rows
	}
	for _, r := range h.hourly {
		if inHourly(plan, w, r.hour) {
			add(r.qid, r.calls, r.execMs, r.rows)
		}
	}
	for _, r := range h.daily {
		if daily[r.day.UTC().Format(time.DateOnly)] {
			add(r.qid, r.calls, r.execMs, r.rows)
		}
	}
	out := make([]qtotal, 0, len(byQID))
	for _, q := range byQID {
		out = append(out, *q)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].qid < out[j].qid })
	return out
}

// groupFamilies groups totals (queryid order) into families and returns
// them with each queryid's family key.
func groupFamilies(totals []qtotal, h history) (map[string]*famAcc, map[int64]string) {
	fams := map[string]*famAcc{}
	famOf := make(map[int64]string, len(totals))
	for _, q := range totals {
		key, cmd, rels := familyParts(q.qid, h.texts[q.qid], h.known)
		f := fams[key]
		if f == nil {
			f = &famAcc{key: key, cmd: cmd, relations: rels}
			fams[key] = f
		}
		f.calls, f.execMs, f.rows = f.calls+q.calls, f.execMs+q.execMs, f.rows+q.rows
		f.qids = append(f.qids, q)
		famOf[q.qid] = key
	}
	return fams, famOf
}

// seenHours are the hourly part's hours with rollup coverage or rows.
func seenHours(plan dayPlan, w Window, h history) map[int64]bool {
	seen := map[int64]bool{}
	for _, t := range h.covered {
		if inHourly(plan, w, t) {
			seen[t.Unix()] = true
		}
	}
	for _, r := range h.hourly {
		if inHourly(plan, w, r.hour) {
			seen[r.hour.Unix()] = true
		}
	}
	return seen
}

// bucketWeeks counts the seen hours of each hour-of-week bucket.
func bucketWeeks(seen map[int64]bool, bw baseline.Window) []int {
	weeks := make([]int, baseline.BucketsPerWeek)
	for unix := range seen {
		weeks[bw.Bucket(time.Unix(unix, 0))]++
	}
	return weeks
}

// shape is a family's profile from its bucket sums and the bucket weeks.
func shape(f *famAcc, weeks []int) Shape {
	s := Shape{CallsMean: make([]float64, baseline.BucketsPerWeek),
		ExecMsMean: make([]float64, baseline.BucketsPerWeek), BucketWeeks: weeks,
		Learning: []int{}}
	for b, n := range weeks {
		s.Weeks = max(s.Weeks, n)
		if n < baseline.MinWeeks {
			s.Learning = append(s.Learning, b)
		}
		if n > 0 {
			s.CallsMean[b] = round12(f.bucketCall[b] / float64(n))
			s.ExecMsMean[b] = round12(f.bucketExec[b] / float64(n))
		}
	}
	return s
}

// rankedFamilies orders families by exec time, then key.
func rankedFamilies(fams map[string]*famAcc) []*famAcc {
	out := make([]*famAcc, 0, len(fams))
	for _, f := range fams {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].execMs != out[j].execMs {
			return out[i].execMs > out[j].execMs
		}
		return out[i].key < out[j].key
	})
	return out
}

// buildWorkload is the workload section and the listed families (for the
// growth section).
func buildWorkload(req Request, plan dayPlan, h history) (Workload, []*famAcc) {
	w := req.Window.UTC()
	totals := queryTotals(plan, w, h)
	fams, famOf := groupFamilies(totals, h)
	bw := baseline.Window{Loc: req.location()}
	for _, r := range h.hourly {
		if f := fams[famOf[r.qid]]; f != nil && inHourly(plan, w, r.hour) {
			b := bw.Bucket(r.hour)
			f.bucketCall[b] += float64(r.calls)
			f.bucketExec[b] += r.execMs
		}
	}
	weeks := bucketWeeks(seenHours(plan, w, h), bw)
	ranked := rankedFamilies(fams)
	out := Workload{Families: []Family{}, FamiliesTotal: len(ranked),
		QueryIDsTotal: len(totals), Order: "exec_ms desc, key"}
	if len(ranked) > MaxFamilies {
		ranked, out.Truncated = ranked[:MaxFamilies], true
	}
	budget := MaxQueryIDs
	for _, f := range ranked {
		fam, cut := familyEntry(f, weeks, &budget, h)
		out.Truncated = out.Truncated || cut
		out.Families = append(out.Families, fam)
	}
	return out, ranked
}

// familyEntry renders f, listing its top queryids within budget; cut is
// true when the budget left some out.
func familyEntry(f *famAcc, weeks []int, budget *int, h history) (Family, bool) {
	sort.Slice(f.qids, func(i, j int) bool {
		if f.qids[i].execMs != f.qids[j].execMs {
			return f.qids[i].execMs > f.qids[j].execMs
		}
		return f.qids[i].qid < f.qids[j].qid
	})
	id := familyID(f.key)
	fam := Family{ID: id, Key: f.key, Command: f.cmd, Relations: f.relations,
		Calls: f.calls, ExecMs: round12(f.execMs), Rows: f.rows,
		QueryIDsTotal: len(f.qids), QueryIDs: []Query{}, Shape: shape(f, weeks)}
	n := min(len(f.qids), TopQueryIDsPerFamily, *budget)
	*budget -= n
	for _, q := range f.qids[:n] {
		_, known := h.texts[q.qid]
		fam.QueryIDs = append(fam.QueryIDs, Query{ID: queryEvidenceID(q.qid),
			QueryID: itoa64(q.qid), Family: id, Calls: q.calls, ExecMs: round12(q.execMs),
			Rows: q.rows, TextKnown: known && strings.TrimSpace(h.texts[q.qid]) != ""})
	}
	return fam, n < min(len(f.qids), TopQueryIDsPerFamily)
}
