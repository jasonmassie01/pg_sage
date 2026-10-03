package schemaguard

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"
)

// Clone-schema families, as the analyzer defines them (dogfood lifeos:
// 160 leaked test_* schemas, each a copy of the same tables). The schema
// guard collapses a family's invariants: an idle family (a leftover) is
// parked once per invariant and never remediated; a live family (e.g.
// schema-per-tenant) is remediated per member and recorded once per
// invariant with the list of affected schemas.

// FamilyMinMembers is the fewest copies that make a family.
const FamilyMinMembers = 5

// minCloneSuffix is the shortest generated suffix (hash, number, date).
const minCloneSuffix = 6

// CloneStem returns a schema name's stem when the name ends in a generated
// suffix (hex digits, digits and separators, at least minCloneSuffix long,
// with a digit), else "".
func CloneStem(name string) string {
	i := len(name)
	for i > 0 && isCloneSuffixByte(name[i-1]) {
		i--
	}
	for i < len(name) && (name[i] == '_' || name[i] == '-') {
		i++
	}
	suffix := name[i:]
	if i == 0 || len(suffix) < minCloneSuffix || !strings.ContainsAny(suffix, "0123456789") {
		return ""
	}
	return name[:i]
}

func isCloneSuffixByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F' ||
		b == '_' || b == '-'
}

// SchemaShape is one schema's tables and its summed scan and tuple
// counters (cumulative since the statistics reset).
type SchemaShape struct {
	Schema   string
	Tables   []string
	Activity int64
}

// GroupFamilies returns the clone families among shapes, keyed by member
// schema; members of one family share one *Family. A family key is the
// stem and a short hash of the sorted table names.
func GroupFamilies(shapes []SchemaShape) map[string]*Family {
	members := map[string][]string{}
	for _, shape := range shapes {
		stem := CloneStem(shape.Schema)
		if stem == "" || len(shape.Tables) == 0 {
			continue
		}
		names := append([]string(nil), shape.Tables...)
		sort.Strings(names)
		sum := sha256.Sum256([]byte(strings.Join(names, "\x1f")))
		key := stem + "*:" + hex.EncodeToString(sum[:4])
		members[key] = append(members[key], shape.Schema)
	}
	result := map[string]*Family{}
	for key, schemas := range members {
		if len(schemas) < FamilyMinMembers {
			continue
		}
		sort.Strings(schemas)
		family := &Family{Key: key, Members: schemas}
		for _, schema := range schemas {
			result[schema] = family
		}
	}
	return result
}

// IdleTracker remembers, per family, its activity and since when it has not
// changed. It is bounded by Retain to the families seen in a cycle.
type IdleTracker struct {
	mu    sync.Mutex
	marks map[string]idleMark
}

type idleMark struct {
	activity int64
	since    time.Time
}

func NewIdleTracker() *IdleTracker {
	return &IdleTracker{marks: map[string]idleMark{}}
}

// QuietFor records activity for key and returns how long it has been
// unchanged (zero on first sight or after a change).
func (t *IdleTracker) QuietFor(key string, activity int64, now time.Time) time.Duration {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	mark, ok := t.marks[key]
	if !ok || mark.activity != activity {
		t.marks[key] = idleMark{activity: activity, since: now}
		return 0
	}
	return now.Sub(mark.since)
}

// Retain forgets every family not in keys.
func (t *IdleTracker) Retain(keys map[string]bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for key := range t.marks {
		if !keys[key] {
			delete(t.marks, key)
		}
	}
}

// IdleEvidence is what decides whether a family is a leftover. A nil
// schema set means the lookup failed: unknown is never evidence of
// idleness.
type IdleEvidence struct {
	Activity         int64
	QuietFor, Window time.Duration
	StatementSchemas map[string]bool
	SessionSchemas   map[string]bool
}

// ClassifyIdle sets family.Idle and family.Reason. Idle needs all of: no
// scan or tuple activity since the statistics reset (or none within the
// window), no pg_stat_statements text naming a member, and a known set of
// sessions none of which uses one.
func ClassifyIdle(family *Family, evidence IdleEvidence) {
	if family == nil {
		return
	}
	family.Idle, family.Reason = false, liveReason(family, evidence)
	if family.Reason == "" {
		family.Idle = true
		family.Reason = "no scans or writes since the statistics reset or within the " +
			"idle window, and no statement or session uses them"
	}
}

func liveReason(family *Family, evidence IdleEvidence) string {
	if evidence.Activity > 0 && evidence.QuietFor < evidence.Window {
		return "tables were scanned or written within the idle window"
	}
	if evidence.StatementSchemas == nil {
		return "statement activity is unknown"
	}
	if member := firstMember(family, evidence.StatementSchemas); member != "" {
		return "pg_stat_statements has statements on " + member
	}
	if evidence.SessionSchemas == nil {
		return "session activity is unknown"
	}
	if member := firstMember(family, evidence.SessionSchemas); member != "" {
		return "a session holds locks in or runs statements on " + member
	}
	return ""
}

func firstMember(family *Family, schemas map[string]bool) string {
	for _, member := range family.Members {
		if schemas[member] {
			return member
		}
	}
	return ""
}
