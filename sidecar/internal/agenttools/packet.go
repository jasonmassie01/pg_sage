package agenttools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// finding is the sage.findings row a packet is built from.
type finding struct {
	ID                                int64
	Category, Severity, Object, Title string
	Recommendation, SQL, Rollback     string
	Detail                            map[string]any
}

const findingSQL = `/* pg_sage */ SELECT id, category, severity,
    COALESCE(object_identifier, ''), title, detail, COALESCE(recommendation, ''),
    COALESCE(recommended_sql, ''), COALESCE(rollback_sql, '')
FROM sage.findings WHERE id = $1`

func (t *Tools) loadFinding(ctx context.Context, id int64) (finding, error) {
	var f finding
	var detail []byte
	err := t.pool.QueryRow(ctx, findingSQL, id).Scan(&f.ID, &f.Category, &f.Severity,
		&f.Object, &f.Title, &detail, &f.Recommendation, &f.SQL, &f.Rollback)
	if errors.Is(err, pgx.ErrNoRows) {
		return finding{}, fmt.Errorf("%w: finding %d", ErrNotFound, id)
	}
	if err != nil {
		return finding{}, fmt.Errorf("read finding %d: %w", id, err)
	}
	dec := json.NewDecoder(bytes.NewReader(detail))
	dec.UseNumber() // query ids exceed float64 precision
	if err := dec.Decode(&f.Detail); err != nil {
		return finding{}, fmt.Errorf("decode finding %d detail: %w", id, err)
	}
	if f.Detail == nil {
		f.Detail = map[string]any{}
	}
	return f, nil
}

// SourceFixPacket builds the source-fix packet of a finding: ErrNotFound
// when there is no such finding, ErrNoChange when it carries no change.
func (t *Tools) SourceFixPacket(ctx context.Context, findingID int64) (Packet, error) {
	if err := t.ready(); err != nil {
		return Packet{}, err
	}
	if findingID <= 0 {
		return Packet{}, invalid("finding_id %d is not positive", findingID)
	}
	f, err := t.loadFinding(ctx, findingID)
	if err != nil {
		return Packet{}, err
	}
	p, err := t.buildPacket(f)
	if err != nil {
		return Packet{}, err
	}
	if p.Database, err = t.currentDatabase(ctx); err != nil {
		return Packet{}, err
	}
	if p.Targets.Sources, err = t.targetSources(ctx, toInt64s(p.Targets.QueryIDs)); err != nil {
		return Packet{}, err
	}
	return p, nil
}

// buildPacket is the packet of f without database reads.
func (t *Tools) buildPacket(f finding) (Packet, error) {
	change, err := findingChange(f)
	if err != nil {
		return Packet{}, err
	}
	ids := detailQueryIDs(f.Detail)
	index := createdIndex(change.Up)
	p := Packet{FindingID: f.ID, Category: safeWord(f.Category),
		Severity: safeWord(f.Severity), Object: safeObject(findingObject(f)),
		Untrusted: untrustedText(f), Evidence: findingEvidence(f), Change: change,
		Targets:     Targets{QueryIDs: toQueryIDs(ids), Objects: targetObjects(f, index)},
		GeneratedAt: t.now()}
	p.Verification = t.verificationPlan(f, change, ids, index)
	p.Problem = problemText(p)
	p.Hash = packetHash(p)
	return p, nil
}

// packetHash identifies what the packet asks for: the finding, the change,
// its targets and prediction (not when it was generated or the window).
func packetHash(p Packet) string {
	pred := p.Verification.Prediction
	expected := "none"
	if pred.ExpectedChangePct != nil {
		expected = strconv.FormatFloat(*pred.ExpectedChangePct, 'g', -1, 64)
	}
	identity := fmt.Sprintf("%d|%q|%q|%q|%t|%v|%q|%q|%q|%s",
		p.FindingID, p.Category, p.Change.Up, p.Change.Down, p.Change.FactBound,
		toInt64s(p.Targets.QueryIDs), p.Targets.Objects, pred.Method, pred.Metric, expected)
	sum := sha256.Sum256([]byte(identity))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// targetSources are the sqlcommenter tags pg_stat_statements kept for the
// target queries. Without pg_stat_statements there are none.
func (t *Tools) targetSources(ctx context.Context, ids []int64) ([]QuerySource, error) {
	out := []QuerySource{}
	if len(ids) == 0 {
		return out, nil
	}
	found, err := t.statementsByID(ctx, ids)
	if errors.Is(err, ErrUnavailable) {
		t.opts.Log("INFO", "agenttools: packet has no query sources: %v", err)
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		s, ok := found[id]
		if !ok {
			continue
		}
		if tags := ParseSQLCommenter(s.Text); len(tags) > 0 {
			out = append(out, QuerySource{QueryID: QueryID(id), Tags: tags,
				ApplicationNames: map[string]int{}})
		}
	}
	return out, nil
}

// problemText states the problem from trusted fields only.
func problemText(p Packet) string {
	what, ok := categoryProblems[p.Category]
	if !ok {
		what = "pg_sage recommends a schema change"
	}
	s := fmt.Sprintf("Finding #%d (%s, severity %s) on %s: %s.", p.FindingID, p.Category,
		p.Severity, p.Object, what)
	if e := p.Verification.ExpectedChangePct; e != nil {
		s += fmt.Sprintf(" Expected effect: %+.1f%% mean execution time of %d target "+
			"queries (%s estimate).", *e, len(p.Targets.QueryIDs), p.Verification.Prediction.Method)
	}
	if p.Change.FactBound {
		s += " A confirmed fact routes this change to the application's migrations."
	}
	return s
}

var categoryProblems = map[string]string{
	"missing_index":        "the target queries have no suitable index",
	"index_recommendation": "the target queries have no suitable index",
	"unused_index":         "an index is not used and costs writes",
	"duplicate_index":      "an index duplicates another one",
	"missing_fk_index":     "a foreign key has no index on its referencing columns",
}

// safeWord keeps a category or severity that is a plain word.
func safeWord(s string) string {
	if wordPattern.MatchString(s) {
		return s
	}
	return "unknown"
}

// safeObject keeps an object name that looks like an identifier; anything
// else could carry instructions and is withheld.
func safeObject(s string) string {
	if s == "" || objectPattern.MatchString(s) {
		return s
	}
	return "(object name withheld: not a plain identifier)"
}

func findingObject(f finding) string {
	if table := detailString(f.Detail, "table"); table != "" {
		return table
	}
	return strings.TrimSpace(f.Object)
}
