// Package publish builds the public outcome bundle (pub-01): what pg_sage
// changed over a window, how the changes were verified and why any were
// reverted, plus the verifier's placebo false-positive rate (lab-02), as
// outcomes.json and a static index.html sealed and signed with the
// evidence key. Every number is computed here from the outcome ledger
// (ux-02a). Database names, hosts, SQL, identifiers and free text never
// enter a bundle: action types, origins and verdicts are kept only when
// they are fixed codes this build knows, and a class or origin with fewer
// than KFloor changes is folded into "other".
package publish

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/outcomes"
	"github.com/pg-sage/sidecar/internal/review/rubric"
	"github.com/pg-sage/sidecar/internal/verify/backtest"
)

// Format names the bundle's layout; a change to outcomes.json bumps it.
const Format = "pg_sage-outcomes/1"

// KFloor is the fewest changes a class or origin needs to be named; fewer
// are counted under OtherKey, so no single database's action stands out.
const KFloor = 5

// Fixed codes the bundle adds to the ledger's vocabulary.
const (
	OtherKey       = "other"   // a folded or unknown class or origin
	UnknownVerdict = "unknown" // a verdict this build does not know
)

// NoChangesStatement is an empty window's statement, in place of zeros.
const NoChangesStatement = "no changes in the window"

// Counts are one bucket's changes. Inconclusive holds the verdicts judged
// without a measurement (inconclusive, unverifiable, expired, unknown).
// Reverted changes were rolled back; RollbackFailed ones tried and failed.
type Counts struct {
	Changes        int `json:"changes"`
	Improved       int `json:"improved"`
	NoChange       int `json:"no_change"`
	Regressed      int `json:"regressed"`
	Inconclusive   int `json:"inconclusive"`
	Pending        int `json:"pending"`
	Reverted       int `json:"reverted"`
	RollbackFailed int `json:"rollback_failed"`
}

// Bucket is one class's or origin's counts.
type Bucket struct {
	Key string `json:"key"`
	Counts
}

// Verifier is the placebo false-positive rate over the newest back-test of
// every database in the bundle, added up (lab-02).
type Verifier struct {
	Backtests      int     `json:"backtests"`
	UsablePlacebos int     `json:"usable_placebos"`
	FalsePositives int     `json:"false_positives"`
	FPRate         float64 `json:"fp_rate"`
	FPWilsonUpper  float64 `json:"fp_wilson_upper"`
}

// Bundle is outcomes.json. Totals, ByClass, ByOrigin and
// RevertedByVerdict are null for an empty window, which Statement says.
type Bundle struct {
	Format            string         `json:"format"`
	Sample            bool           `json:"sample,omitempty"`
	RubricVersion     string         `json:"rubric_version"`
	GeneratedAt       time.Time      `json:"generated_at"`
	Since             string         `json:"since"`
	From              time.Time      `json:"from"`
	To                time.Time      `json:"to"`
	Databases         int            `json:"databases"`
	KFloor            int            `json:"k_floor"`
	Statement         string         `json:"statement"`
	Totals            *Counts        `json:"totals"`
	ByClass           []Bucket       `json:"by_class"`
	ByOrigin          []Bucket       `json:"by_origin"`
	RevertedByVerdict map[string]int `json:"reverted_by_verdict"`
	Verifier          *Verifier      `json:"verifier"`
	VerifierStatement string         `json:"verifier_statement,omitempty"`
}

// Input is what a bundle is built from: the window's ledger rows of every
// database, and the newest v2 back-test counts of each that has one.
type Input struct {
	Rows      []outcomes.Row
	Databases int
	Backtests []backtest.Counts
	Since     time.Duration
	Now       time.Time
	Sample    bool
}

// Build computes the bundle of in.
func Build(in Input) Bundle {
	now := in.Now.UTC()
	b := Bundle{Format: Format, Sample: in.Sample, RubricVersion: rubric.Version,
		GeneratedAt: now, Since: FormatSince(in.Since), From: now.Add(-in.Since), To: now,
		Databases: in.Databases, KFloor: KFloor, Statement: NoChangesStatement}
	b.Verifier, b.VerifierStatement = verifierOf(in.Backtests)
	if len(in.Rows) == 0 {
		return b
	}
	var totals Counts
	classes, origins := map[string]Counts{}, map[string]Counts{}
	b.RevertedByVerdict = map[string]int{}
	for _, r := range in.Rows {
		verdict := verdictCode(r.Verdict)
		totals = add(totals, verdict, r.RollbackState)
		class, origin := classCode(r.ActionType), originCode(r.Origin)
		classes[class] = add(classes[class], verdict, r.RollbackState)
		origins[origin] = add(origins[origin], verdict, r.RollbackState)
		if r.RollbackState == outcomes.RollbackRolledBack {
			b.RevertedByVerdict[verdict]++
		}
	}
	b.Totals, b.ByClass, b.ByOrigin = &totals, fold(classes), fold(origins)
	b.Statement = statement(totals)
	return b
}

// add counts one change of verdict and rollback state into c.
func add(c Counts, verdict, rollback string) Counts {
	c.Changes++
	switch verdict {
	case outcomes.VerdictImproved:
		c.Improved++
	case outcomes.VerdictNoChange:
		c.NoChange++
	case outcomes.VerdictRegressed:
		c.Regressed++
	case outcomes.VerdictPending:
		c.Pending++
	default:
		c.Inconclusive++
	}
	switch rollback {
	case outcomes.RollbackRolledBack:
		c.Reverted++
	case outcomes.RollbackFailed:
		c.RollbackFailed++
	}
	return c
}

func statement(c Counts) string {
	return fmt.Sprintf("%d changes in the window: %d verified improved, %d no change, "+
		"%d regressed, %d inconclusive, %d not judged yet; %d reverted", c.Changes,
		c.Improved, c.NoChange, c.Regressed, c.Inconclusive, c.Pending, c.Reverted)
}

func verifierOf(tests []backtest.Counts) (*Verifier, string) {
	if len(tests) == 0 {
		return nil, "the verifier has not been back-tested on these databases"
	}
	var sum backtest.Counts
	for _, c := range tests {
		sum.Placebos += c.Placebos
		sum.Excluded += c.Excluded
		sum.FalsePositives += c.FalsePositives
	}
	if sum.Usable() <= 0 {
		return nil, "the back-tests had no usable placebos"
	}
	return &Verifier{Backtests: len(tests), UsablePlacebos: sum.Usable(),
		FalsePositives: sum.FalsePositives, FPRate: roundTo(sum.FPRate(), 4),
		FPWilsonUpper: roundTo(sum.FPWilsonUpper(), 4)}, ""
}

func roundTo(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// maxDays is the ledger's retention (outcomes.MaxSince), in days.
var maxDays = int(outcomes.MaxSince / (24 * time.Hour))

var errSince = fmt.Errorf("--since must be a whole number of days from 1d to %dd, or a "+
	"duration such as 36h within that", maxDays)

// ParseSince parses a window: whole days such as 90d, or a Go duration
// such as 36h, from one second to the ledger's 400-day retention.
func ParseSince(raw string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 1 || n > maxDays || strings.HasPrefix(days, "+") {
			return 0, errSince
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	if raw == "" {
		return 0, errSince
	}
	d, err := outcomes.ParseSince(raw)
	if err != nil {
		return 0, errors.Join(errSince, err)
	}
	return d, nil
}

// FormatSince renders a window as whole days (90d) when it is one, else as
// hours (36h).
func FormatSince(d time.Duration) string {
	day := 24 * time.Hour
	if d > 0 && d%day == 0 {
		return strconv.Itoa(int(d/day)) + "d"
	}
	return outcomes.FormatSince(d)
}
