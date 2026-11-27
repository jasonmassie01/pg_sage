package api

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/fleet"
)

// Fleet list (ux-10a): GET /api/v1/databases with any of q, tag, sort,
// order, limit or cursor searches, filters, sorts and pages the rows of
// FleetStatus in Go, after the caller's grant filter (aggregateRead). The
// summary still counts every database the caller can see; total counts the
// matches. Without these parameters the response is today's, unchanged.
const (
	fleetListDefaultLimit = 50
	fleetListMaxLimit     = 500
)

var fleetListParams = []string{"q", "tag", "sort", "order", "limit", "cursor"}

// fleetSorts maps a sort to its default order. Name and mode read A to Z;
// health and last snapshot put the worst (lowest score, oldest collection)
// first; open issues put the most first.
var fleetSorts = map[string]string{
	"name": "asc", "health": "asc", "open_issues": "desc",
	"last_snapshot": "asc", "mode": "asc",
}

// fleetListQuery is a parsed list request.
type fleetListQuery struct {
	q     string
	tags  []fleetTag
	sort  string
	order string
	limit int
	after *fleetKey
}

// fleetTag is one tag filter: key=value, or a bare tag (value unset).
type fleetTag struct {
	key, value string
	bare       bool
}

// fleetKey is a row's position: its sort value (number or text), then
// its name, which is unique and keeps the order total.
type fleetKey struct {
	n    int64
	s    string
	name string
}

// fleetListResponse is FleetOverview plus the paging fields that
// web/src/lib/listPaging.js reads.
type fleetListResponse struct {
	fleet.FleetOverview
	Total       int    `json:"total"`
	TotalCapped bool   `json:"total_capped"`
	NextCursor  string `json:"next_cursor"`
}

// isFleetListRequest is true when q names any list parameter.
func isFleetListRequest(q url.Values) bool {
	for _, p := range fleetListParams {
		if _, ok := q[p]; ok {
			return true
		}
	}
	return false
}

func parseFleetListQuery(q url.Values) (fleetListQuery, error) {
	lq := fleetListQuery{q: strings.ToLower(strings.TrimSpace(q.Get("q"))),
		sort: valOrDefault(q, "sort", "name")}
	def, ok := fleetSorts[lq.sort]
	if !ok {
		return lq, fmt.Errorf("sort %q is not one of name, health, open_issues, "+
			"last_snapshot, mode", lq.sort)
	}
	lq.order = valOrDefault(q, "order", def)
	if lq.order != "asc" && lq.order != "desc" {
		return lq, fmt.Errorf("order %q must be asc or desc", lq.order)
	}
	limit, err := parseFleetLimit(q.Get("limit"))
	if err != nil {
		return lq, err
	}
	lq.limit = limit
	if lq.tags, err = parseFleetTags(q["tag"]); err != nil {
		return lq, err
	}
	if raw := q.Get("cursor"); raw != "" {
		if lq.after, err = parseFleetCursor(raw, lq.sort, lq.order); err != nil {
			return lq, err
		}
	}
	return lq, nil
}

func parseFleetLimit(raw string) (int, error) {
	if raw == "" {
		return fleetListDefaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > fleetListMaxLimit {
		return 0, fmt.Errorf("limit %q must be a number from 1 to %d", raw, fleetListMaxLimit)
	}
	return n, nil
}

// parseFleetTags reads each tag filter as key=value or a bare tag, trimmed
// and case-insensitive.
func parseFleetTags(raw []string) ([]fleetTag, error) {
	out := make([]fleetTag, 0, len(raw))
	for _, r := range raw {
		key, value, hasValue := strings.Cut(strings.ToLower(r), "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key == "" {
			return nil, fmt.Errorf("tag %q must be key=value or a bare tag", r)
		}
		out = append(out, fleetTag{key: key, value: value, bare: !hasValue})
	}
	return out, nil
}

// parseFleetCursor decodes a cursor issued by this list for sort and order.
func parseFleetCursor(raw, sortName, order string) (*fleetKey, error) {
	c, err := decodeListCursor(raw)
	if err != nil {
		return nil, err
	}
	if c.Sort != sortName || c.Order != order {
		return nil, errors.New("cursor was issued for another sort order")
	}
	if len(c.Keys) != 2 || c.Keys[1] == "" {
		return nil, errors.New("cursor is not valid for the database list")
	}
	k := &fleetKey{s: c.Keys[0], name: c.Keys[1]}
	if fleetSortIsNumeric(sortName) {
		if k.n, err = strconv.ParseInt(c.Keys[0], 10, 64); err != nil {
			return nil, errors.New("cursor is not valid for the database list")
		}
		k.s = ""
	}
	return k, nil
}

func fleetSortIsNumeric(sortName string) bool {
	return sortName == "health" || sortName == "open_issues" || sortName == "last_snapshot"
}

func encodeFleetCursor(k fleetKey, sortName, order string) string {
	first := k.s
	if fleetSortIsNumeric(sortName) {
		first = strconv.FormatInt(k.n, 10)
	}
	return encodeListCursor(listCursor{Sort: sortName, Order: order,
		Keys: []string{first, k.name}})
}

// listFleet filters, sorts and pages ov's rows; facets carry each row's
// host and execution mode.
func listFleet(ov fleet.FleetOverview, facets map[string]fleet.DatabaseFacets,
	lq fleetListQuery) fleetListResponse {
	type row struct {
		db  fleet.DatabaseStatus
		key fleetKey
	}
	matched := make([]row, 0, len(ov.Databases))
	for _, db := range ov.Databases {
		f := facets[db.Name]
		if fleetRowMatches(db, f, lq) {
			matched = append(matched, row{db: db, key: fleetSortKey(db, f, lq.sort)})
		}
	}
	desc := lq.order == "desc"
	sort.Slice(matched, func(i, j int) bool {
		return compareFleetKeys(matched[i].key, matched[j].key, desc) < 0
	})
	resp := fleetListResponse{FleetOverview: ov, Total: len(matched)}
	resp.Databases = make([]fleet.DatabaseStatus, 0, min(lq.limit, len(matched)))
	start := 0
	if lq.after != nil {
		start = sort.Search(len(matched), func(i int) bool {
			return compareFleetKeys(matched[i].key, *lq.after, desc) > 0
		})
	}
	end := min(start+lq.limit, len(matched))
	for _, r := range matched[start:end] {
		resp.Databases = append(resp.Databases, r.db)
	}
	if end < len(matched) {
		resp.NextCursor = encodeFleetCursor(matched[end-1].key, lq.sort, lq.order)
	}
	return resp
}

// compareFleetKeys orders by sort value (reversed when desc), then by name
// ascending.
func compareFleetKeys(a, b fleetKey, desc bool) int {
	c := cmpInt64(a.n, b.n)
	if c == 0 {
		c = strings.Compare(a.s, b.s)
	}
	if desc {
		c = -c
	}
	if c == 0 {
		c = strings.Compare(a.name, b.name)
	}
	return c
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func fleetSortKey(db fleet.DatabaseStatus, f fleet.DatabaseFacets, sortName string) fleetKey {
	k := fleetKey{name: db.Name}
	st := db.Status
	if st == nil {
		st = &fleet.InstanceStatus{}
	}
	switch sortName {
	case "name":
		k.s = db.Name
	case "health":
		k.n = int64(st.HealthScore)
	case "open_issues":
		k.n = int64(st.FindingsOpen)
	case "last_snapshot":
		k.n = snapshotKey(st.CollectorLastRun)
	case "mode":
		k.s = f.ExecutionMode
	}
	return k
}

// snapshotKey is a collection time as a sort value. A database never
// collected (zero time) is the oldest of all; UnixNano is undefined for it.
func snapshotKey(t time.Time) int64 {
	if t.IsZero() {
		return math.MinInt64
	}
	return t.UnixNano()
}

// fleetRowMatches is true when the row's name or host contains q (case
// insensitive, literal text) and it carries every tag filter.
func fleetRowMatches(db fleet.DatabaseStatus, f fleet.DatabaseFacets, lq fleetListQuery) bool {
	if lq.q != "" && !strings.Contains(strings.ToLower(db.Name), lq.q) &&
		!strings.Contains(strings.ToLower(f.Host), lq.q) {
		return false
	}
	for _, want := range lq.tags {
		if !hasFleetTag(db.Tags, want) {
			return false
		}
	}
	return true
}

// hasFleetTag matches key=value against a tag with that key and value, and
// a bare filter against a bare tag or any tag with that key.
func hasFleetTag(tags []string, want fleetTag) bool {
	for _, t := range tags {
		key, value, hasValue := strings.Cut(strings.ToLower(t), "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key != want.key {
			continue
		}
		if want.bare || (hasValue && value == want.value) {
			return true
		}
	}
	return false
}
