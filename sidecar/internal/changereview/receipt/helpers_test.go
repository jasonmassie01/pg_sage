package receipt

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/changefeed"
	"github.com/pg-sage/sidecar/internal/verify"
)

const testPR = "https://github.com/acme/shop/pull/7"

// baseReview is a complete, awaiting, safe review of the fixture's
// database with no expectations; seed adjusts it.
func (f *fixture) baseReview() changereview.Review {
	now := f.clock.Now().UTC().Truncate(time.Microsecond)
	return changereview.Review{Database: f.name, Source: changereview.SourceMCP,
		Actor: "mcp:token:ci", InputHash: randomHex(64), ContentHash: randomHex(64),
		State: changereview.StateComplete, Stage: changereview.StageDone,
		Verdict: changereview.VerdictSafe, DeterministicVerdict: changereview.VerdictSafe,
		ParseMode: changereview.ParseAST, ModelStatus: changereview.ModelNoModel,
		Statements: []changereview.Statement{}, Findings: []changereview.Finding{},
		Evidence: []changereview.Evidence{}, Dropped: []changereview.Dropped{},
		NotDetermined: []changereview.NotDetermined{},
		Expectations: changereview.Expectations{
			PostConditions: []changereview.PostCondition{}, WatchQueryIDs: []int64{},
			Relations: []string{}, Predictions: []verify.Prediction{}},
		LandingState: changereview.LandingAwaiting, ReceiptVerdict: changereview.ReceiptPending,
		CreditState: changereview.CreditNone, ShadowScore: changereview.ShadowUnscored,
		CreatedAt: now.Add(-time.Hour), CompletedAt: ptr(now.Add(-59 * time.Minute)),
		UpdatedAt: now.Add(-59 * time.Minute)}
}

// seed inserts a review built from baseReview and edit.
func (f *fixture) seed(edit func(*changereview.Review)) changereview.Review {
	f.t.Helper()
	r := f.baseReview()
	if edit != nil {
		edit(&r)
	}
	out, err := f.svc.Store().Insert(f.ctx, r)
	if err != nil {
		f.t.Fatalf("seed review: %v", err)
	}
	return out
}

// landedAt makes a review landed by source between lower and at.
func landedAt(r *changereview.Review, source string, lower, at time.Time) {
	r.LandingState, r.LandingSource = changereview.LandingLanded, source
	l, a := lower.UTC().Truncate(time.Microsecond), at.UTC().Truncate(time.Microsecond)
	r.LandedLower, r.LandedAt = &l, &a
}

// watching sets the review's watch set with a no-change prediction per id
// and the touched relations.
func watching(r *changereview.Review, rels []string, ids ...int64) {
	zero := 0.0
	r.Expectations.WatchQueryIDs = append([]int64{}, ids...)
	r.Expectations.Relations = append([]string{}, rels...)
	r.Expectations.Predictions = []verify.Prediction{}
	for _, id := range ids {
		r.Expectations.Predictions = append(r.Expectations.Predictions,
			verify.Prediction{Class: "change_review", Method: verify.MethodRule,
				Metric: verify.MetricMeanExecTime, TargetQueryIDs: []int64{id},
				ExpectedChangePct: &zero})
	}
}

// concurred records a DBA concur directly in the seeded row (pending
// credit), as Store.Decide leaves it.
func concurred(r *changereview.Review) {
	at := r.CompletedAt.Add(time.Minute)
	r.DBAVerdict, r.DBAActor, r.DBADecidedAt = changereview.DBAConcur, "user:1", &at
	r.CreditState, r.CreditedMinutes, r.ToilModelVersion = changereview.CreditPending, 15,
		ptr(1)
}

// counters are a query's cumulative pg_stat_statements counters.
type counters struct {
	calls int64
	total float64
}

// series writes query_store samples of qid: n samples gap apart from
// start, each adding calls calls at meanMs (alternating +-2%), on top of
// the cumulative counters base. It returns the counters after the last.
func (f *fixture) series(qid int64, start time.Time, n int, gap time.Duration, calls int64,
	meanMs float64, base counters) counters {
	f.t.Helper()
	st := histstore.Resolve(f.pool)
	c := base
	for i := 0; i < n; i++ {
		m := meanMs * 1.02
		if i%2 == 1 {
			m = meanMs * 0.98
		}
		c.calls += calls
		c.total += float64(calls) * m
		if _, err := st.Exec(f.ctx, `INSERT INTO sage.query_store
			(captured_at, queryid, calls, total_exec_time, mean_exec_time{dbcol})
			VALUES ($1, $2, $3, $4, $5{dbval})`, start.Add(time.Duration(i)*gap), qid,
			c.calls, c.total, c.total/float64(c.calls)); err != nil {
			f.t.Fatalf("seed query_store sample: %v", err)
		}
	}
	return c
}

// beforeAfter seeds qid around a landing: 20 samples every 30 minutes in
// the 24 h before lower and 20 in the 24 h after at, before at meanBefore
// and after at meanAfter, with callsPer calls per sample.
func (f *fixture) beforeAfter(qid int64, lower, at time.Time, callsPer int64, meanBefore,
	meanAfter float64) {
	f.t.Helper()
	c := f.series(qid, lower.Add(-22*time.Hour), 20, 30*time.Minute, callsPer, meanBefore,
		counters{})
	f.series(qid, at.Add(time.Hour), 20, 30*time.Minute, callsPer, meanAfter, c)
}

// event records a change event: external kinds through signed ingestion,
// pg_sage's own kinds as its pollers record them.
func (f *fixture) event(kind changefeed.Kind, link string, at time.Time) changefeed.Event {
	f.t.Helper()
	if kind.External() {
		e, _, err := f.feed.Ingest(f.ctx, changefeed.Submission{Source: "ci",
			EventID: "ev-" + string(sre.NewUUID()), Kind: kind, Database: f.name,
			Summary: "deployed", Link: link, OccurredAt: at}, time.Now())
		if err != nil {
			f.t.Fatalf("ingest %s event: %v", kind, err)
		}
		return e
	}
	e := changefeed.Event{Source: changefeed.SourceSage, EventID: "own-" +
		string(sre.NewUUID()), Kind: kind, Scoped: true, Summary: string(kind),
		Objects: []string{}, OccurredAt: at.UTC(), ReceivedAt: time.Now().UTC(),
		Signature: changefeed.SignatureInternal}
	sum := sha256.Sum256([]byte(e.EventID))
	e.Hash = hex.EncodeToString(sum[:])
	out, _, err := f.events.Record(f.ctx, f.scope, true, e)
	if err != nil {
		f.t.Fatalf("record %s event: %v", kind, err)
	}
	return out
}

// incident inserts an open incident on objects detected at at.
func (f *fixture) incident(at time.Time, objects ...string) string {
	f.t.Helper()
	var id string
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.incidents (detected_at,
		last_detected_at, severity, root_cause, affected_objects, source, database_name)
		VALUES ($1, $1, 'critical', 'lock contention', $2, 'deterministic', $3)
		RETURNING id::text`, at, objects, f.name).Scan(&id); err != nil {
		f.t.Fatalf("seed incident: %v", err)
	}
	return id
}

// lockFinding inserts a lock_chain finding on relation seen at at.
func (f *fixture) lockFinding(at time.Time, relation string) int64 {
	f.t.Helper()
	var id int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.findings (created_at, last_seen,
		category, severity, object_type, object_identifier, title, detail, status)
		VALUES ($1, $1, 'lock_chain', 'critical', 'lock', 'pid:' || $3::text,
		'Lock chain', jsonb_build_object('locked_relation', $2::text), 'resolved')
		RETURNING id`, at, relation, at.UnixNano()%100000).Scan(&id); err != nil {
		f.t.Fatalf("seed lock finding: %v", err)
	}
	return id
}
