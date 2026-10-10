package agentposture

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func init() { Register(ap10{}) }

// ap10 reports software agents make dangerous to leave unpatched:
// pgvector below the releases that fix its HNSW vacuum and IVFFlat build
// bugs (critical, when indexes of that kind exist) and a server major at
// or within 90 days of its end of life (warning).
type ap10 struct{}

func (ap10) Spec() Spec {
	return Spec{ID: "AP-10", Title: "Vulnerable pgvector and end-of-life server versions",
		Severity: Critical}
}

// postgresEOL is each PostgreSQL major's final release (end of life),
// from the versioning policy at https://www.postgresql.org/support/versioning/
// (read 2026-10-09; spec §6.6 also gives PG14's). Majors older than the
// table are past end of life; newer ones are unknown and not reported.
var postgresEOL = map[int]string{
	13: "2025-11-13",
	14: "2026-11-12",
	15: "2027-11-11",
	16: "2028-11-09",
	17: "2029-11-08",
	18: "2030-11-14",
}

// eolWindow is how far ahead of end of life the server is reported.
const eolWindow = 90 * 24 * time.Hour

// pgvectorFix is a pgvector bug fixed in a release, for indexes of one
// access method. The fixed releases are those of the spec's competitive
// research (§1.6); no CVE id is cited because none is verified.
type pgvectorFix struct {
	am, fixed, issue string
}

var pgvectorFixes = []pgvectorFix{
	{am: "hnsw", fixed: "0.8.4", issue: "HNSW index vacuum bugs"},
	{am: "ivfflat", fixed: "0.8.7", issue: "an IVFFlat index build overflow"},
}

var ap10ExtensionSQL = Statement("AP-10", `SELECT e.extversion FROM pg_catalog.pg_extension e
WHERE e.extname = 'vector'`)

var ap10IndexSQL = Statement("AP-10", `SELECT a.amname::text,
  pg_catalog.format('%I.%I', n.nspname, c.relname)
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_am a ON a.oid = c.relam
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'i' AND a.amname IN ('hnsw', 'ivfflat')
ORDER BY 1, 2
LIMIT $1`)

func (ap10) Detect(ctx context.Context, in Input) ([]Finding, error) {
	var out []Finding
	if f, ok := eolFinding(in.Env.VersionNum, time.Now()); ok {
		out = append(out, f)
	}
	var version string
	err := in.Q.QueryRow(ctx, ap10ExtensionSQL).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the pgvector version: %w", err)
	}
	idx, err := vectorIndexes(ctx, in.Q)
	if err != nil {
		return nil, err
	}
	return append(out, pgvectorFindings(version, idx)...), nil
}

// vectorIndexes lists the HNSW and IVFFlat indexes by access method.
func vectorIndexes(ctx context.Context, q Querier) (map[string][]string, error) {
	rows, err := q.Query(ctx, ap10IndexSQL, apbMaxRows)
	if err != nil {
		return nil, fmt.Errorf("read vector indexes: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var am, name string
		if err := rows.Scan(&am, &name); err != nil {
			return nil, fmt.Errorf("read vector indexes: %w", err)
		}
		out[am] = append(out[am], name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read vector indexes: %w", err)
	}
	return out, nil
}

// pgvectorFindings reports each fix the installed version lacks while
// indexes of its kind exist. An unparsable version is not guessed at.
func pgvectorFindings(version string, indexes map[string][]string) []Finding {
	have, ok := parseExtVersion(version)
	if !ok {
		return nil
	}
	var out []Finding
	for _, fix := range pgvectorFixes {
		want, _ := parseExtVersion(fix.fixed)
		idx := indexes[fix.am]
		if len(idx) == 0 || !have.less(want) {
			continue
		}
		out = append(out, Finding{Severity: Critical, ObjectType: "extension",
			Object: "vector/" + fix.am,
			Title:  "pgvector " + version + " has " + fix.issue,
			Detail: fmt.Sprintf("pgvector %s is below %s, which fixes %s, and %d %s "+
				"indexes exist: %s.", version, fix.fixed, fix.issue, len(idx), fix.am,
				listSome(idx, 10)),
			Recommendation: "Install pgvector " + fix.fixed + " or later on the server, " +
				"then update the extension; rebuild the affected indexes if the release " +
				"notes ask for it.",
			FixScript: "-- after installing pgvector " + fix.fixed + "+ on the server:\n" +
				"ALTER EXTENSION vector UPDATE;",
			Caveat: "Fixed releases are from pg_sage's research of pgvector's changelog; " +
				"confirm them against the release notes.",
			Evidence: []Evidence{{Source: "pg_extension", Ref: "vector",
				Detail: "extversion " + version},
				{Source: "pg_class", Ref: fix.am + " indexes", Detail: listSome(idx, 10)}}})
	}
	return out
}

// listSome is the first n items, then how many more there are.
func listSome(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" and %d more", len(items)-n)
}

// eolFinding reports a server major at or within eolWindow of its end of
// life on now's date; majors older than the table are past it.
func eolFinding(versionNum int, now time.Time) (Finding, bool) {
	major := versionNum / 10000
	date, known := postgresEOL[major]
	if major == 0 || (!known && major >= 13) {
		return Finding{}, false
	}
	object := "PostgreSQL " + strconv.Itoa(major)
	when := "is past end of life (before PostgreSQL 13's, " + postgresEOL[13] + ")"
	if known {
		end, _ := time.Parse("2006-01-02", date)
		if end.Sub(now) > eolWindow {
			return Finding{}, false
		}
		when = "reached end of life on " + date
		if now.Before(end) {
			when = "reaches end of life on " + date
		}
	}
	return Finding{Severity: Warning, ObjectType: "server", Object: object,
		Title:  object + " is at or near end of life",
		Detail: object + " " + when + "; after it, no security or bug fixes are released.",
		Recommendation: "Plan the upgrade to a supported major (pg_upgrade or logical " +
			"replication); test it on a copy first.",
		FixScript: "-- Upgrade to a supported major; check the cluster first:\n" +
			"-- pg_upgrade --check -b <old bin> -B <new bin> -d <old data> -D <new data>",
		Evidence: []Evidence{{Source: "server_version_num", Ref: strconv.Itoa(versionNum),
			Detail: object + " " + when}}}, true
}

// extVersion is a major.minor.patch extension version.
type extVersion [3]int

// parseExtVersion reads "0.8.2", "0.8" or "1.0.0-beta" (the suffix is
// ignored).
func parseExtVersion(s string) (extVersion, bool) {
	if i := strings.IndexAny(s, "-+ "); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return extVersion{}, false
	}
	var v extVersion
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" || p[0] == '+' {
			return extVersion{}, false
		}
		v[i] = n
	}
	return v, true
}

func (v extVersion) less(w extVersion) bool {
	for i := range v {
		if v[i] != w[i] {
			return v[i] < w[i]
		}
	}
	return false
}
