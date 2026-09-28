package recommendation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const (
	backoffBase = 5 * time.Minute
	backoffCap  = 6 * time.Hour
)

// Backoff is the wait before retry attempt+1 after attempt failed:
// 5 minutes doubling per attempt, capped at 6 hours.
func Backoff(attempt int) time.Duration {
	wait := backoffBase
	for i := 1; i < attempt && wait < backoffCap; i++ {
		wait *= 2
	}
	if wait > backoffCap {
		return backoffCap
	}
	return wait
}

// actionPrefixes classify a statement by its leading keywords.
var actionPrefixes = []struct{ prefix, action string }{
	{"create index", "create_index"},
	{"create unique index", "create_index"},
	{"drop index", "drop_index"},
	{"reindex", "reindex"},
	{"vacuum", "vacuum"},
	{"analyze", "analyze"},
	{"alter system", "alter_system"},
	{"alter table", "alter_table"},
}

// ActionType classifies sql for the recommendation identity.
func ActionType(sql string) string {
	norm := normalizeSQL(sql)
	for _, p := range actionPrefixes {
		if strings.HasPrefix(norm, p.prefix) {
			return p.action
		}
	}
	switch {
	case strings.Contains(norm, "pg_terminate_backend"):
		return "terminate_backend"
	case strings.Contains(norm, "pg_cancel_backend"):
		return "cancel_backend"
	default:
		return "other"
	}
}

// IndexFingerprint identifies the index a statement creates or drops
// (C05): a CREATE INDEX by its definition (table, method, keys,
// predicate; not its name), a DROP/REINDEX by the object it names. It is
// empty for every other statement.
func IndexFingerprint(sql string) string {
	norm := normalizeSQL(sql)
	switch ActionType(sql) {
	case "create_index":
		on := strings.Index(norm, " on ")
		if on < 0 {
			return ""
		}
		unique := strings.HasPrefix(norm, "create unique")
		return digest("create", strconv.FormatBool(unique), norm[on+4:])
	case "drop_index":
		return digest("drop", firstObject(norm, "drop index"))
	case "reindex":
		return digest("reindex", strings.TrimPrefix(norm, "reindex "))
	default:
		return ""
	}
}

// firstObject returns the object named after keyword, skipping IF EXISTS
// (normalizeSQL already removed CONCURRENTLY).
func firstObject(norm, keyword string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(norm, keyword))
	rest = strings.TrimPrefix(rest, "if exists ")
	if fields := strings.Fields(rest); len(fields) > 0 {
		return fields[0]
	}
	return ""
}

// IdentityKey is the stable identity of the recommendation p proposes:
// database, category, canonical target, action type and index fingerprint.
func IdentityKey(p Proposal) string {
	return digest(p.DatabaseName, p.Category, strings.TrimSpace(p.Target),
		ActionType(p.ForwardSQL), IndexFingerprint(p.ForwardSQL))
}

// preconditions are the facts the content hash pins beyond the SQL: the
// target, the action and index identity, and whether an inverse exists.
func preconditions(p Proposal) map[string]any {
	return map[string]any{
		"target":            strings.TrimSpace(p.Target),
		"action_type":       ActionType(p.ForwardSQL),
		"index_fingerprint": IndexFingerprint(p.ForwardSQL),
		"has_inverse":       strings.TrimSpace(p.InverseSQL) != "",
	}
}

// ContentHash covers exactly what an approval approves: identity, forward
// SQL, inverse SQL and preconditions. Evidence, wording and the policy
// version are recorded on the revision but do not change the hash, so
// fresh metrics never invalidate an approval.
func ContentHash(p Proposal) string {
	pre, _ := json.Marshal(preconditions(p)) // map keys marshal sorted
	return digest(IdentityKey(p), strings.TrimSpace(p.ForwardSQL),
		strings.TrimSpace(p.InverseSQL), string(pre))
}

// digest hashes length-prefixed fields, so field boundaries are exact.
func digest(fields ...string) string {
	h := sha256.New()
	for _, f := range fields {
		h.Write([]byte(strconv.Itoa(len(f))))
		h.Write([]byte{':'})
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// normalizeSQL lower-cases and collapses whitespace outside double-quoted
// identifiers and drops a trailing semicolon.
func normalizeSQL(sql string) string {
	var b strings.Builder
	quoted, space := false, false
	for _, r := range strings.TrimSpace(sql) {
		switch {
		case r == '"':
			quoted = !quoted
		case !quoted && (r == ' ' || r == '\t' || r == '\n' || r == '\r'):
			space = true
			continue
		case !quoted && r >= 'A' && r <= 'Z':
			r += 'a' - 'A'
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	out := strings.TrimSuffix(b.String(), ";")
	out = strings.ReplaceAll(out, " if not exists", "")
	return strings.Replace(out, " concurrently", "", 1)
}
