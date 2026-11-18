package capacity

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/forecaster"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// MinPoints is the fewest daily sizes a projection is made from.
const MinPoints = 7

// horizons are the days ahead a growth plan states.
var horizons = [...]int{30, 90, 365}

// ErrTooFewPoints is a series with fewer than MinPoints measured days.
var ErrTooFewPoints = errors.New("too few daily sizes to project")

// Ahead is a projected size Days after the last measured day, and its
// band: twice the standard error of a prediction there (about 95%).
type Ahead struct {
	Days  int
	Bytes float64
	Band  float64
}

// Projection is a least-squares line through a daily size series (x is
// the day's index in the window, so a missing day keeps its place).
// Origin is the UTC day of the last measured point; RMSE is the residual
// standard error, sqrt(SSR / (n - 2)). A projected size is never below 0.
type Projection struct {
	Series      string
	Table       string
	Points      int
	Origin      time.Time
	LastBytes   float64
	SlopePerDay float64
	RMSE        float64
	Ahead       []Ahead
}

// Shrinking reports a falling fit (data deleted, or archived).
func (p Projection) Shrinking() bool { return p.SlopePerDay < 0 }

// At is the projection days ahead (ok false for a horizon not stated).
func (p Projection) At(days int) (Ahead, bool) {
	for _, a := range p.Ahead {
		if a.Days == days {
			return a, true
		}
	}
	return Ahead{}, false
}

// Project fits s, whose first value is the UTC day of from, and projects
// it 30, 90 and 365 days past its last measured day. A negative or
// non-finite size is an error, and so are fewer than MinPoints points
// (ErrTooFewPoints).
func Project(s workpack.Series, from time.Time) (Projection, error) {
	pts, err := points(s)
	if err != nil {
		return Projection{}, err
	}
	if len(pts) < MinPoints {
		return Projection{}, fmt.Errorf("%w: %s has %d daily points, needs %d",
			ErrTooFewPoints, s.ID, len(pts), MinPoints)
	}
	reg := forecaster.LinearRegression(pts)
	f := newFitError(pts, reg)
	last := pts[len(pts)-1]
	p := Projection{Series: s.ID, Table: s.Table, Points: len(pts),
		Origin:    dayStart(from).AddDate(0, 0, int(last.X)),
		LastBytes: last.Y, SlopePerDay: reg.Slope, RMSE: f.rmse}
	for _, h := range horizons {
		x := last.X + float64(h)
		p.Ahead = append(p.Ahead, Ahead{Days: h,
			Bytes: math.Max(0, reg.Intercept+reg.Slope*x), Band: f.band(x)})
	}
	return p, nil
}

// points are s's measured days; a bad size is an error naming its day.
func points(s workpack.Series) ([]forecaster.DataPoint, error) {
	var pts []forecaster.DataPoint
	for i, v := range s.Values {
		switch {
		case v == nil:
			continue
		case math.IsNaN(*v) || math.IsInf(*v, 0):
			return nil, fmt.Errorf("growth %s: day %d size is not a number", s.ID, i)
		case *v < 0:
			return nil, fmt.Errorf("growth %s: day %d size %.0f is negative", s.ID, i, *v)
		}
		pts = append(pts, forecaster.DataPoint{X: float64(i), Y: *v})
	}
	return pts, nil
}

// fitError is what a prediction's standard error needs of a fit.
type fitError struct {
	rmse, meanX, sxx float64
	n                int
}

func newFitError(pts []forecaster.DataPoint, reg forecaster.RegressionResult) fitError {
	f := fitError{n: len(pts)}
	for _, p := range pts {
		f.meanX += p.X
	}
	f.meanX /= float64(f.n)
	var ssr float64
	for _, p := range pts {
		r := p.Y - (reg.Slope*p.X + reg.Intercept)
		if math.Abs(r) <= 1e-9*math.Max(1, math.Abs(p.Y)) {
			r = 0 // float noise of an exact line, not a residual
		}
		ssr += r * r
		f.sxx += (p.X - f.meanX) * (p.X - f.meanX)
	}
	f.rmse = math.Sqrt(ssr / float64(f.n-2))
	return f
}

// band is twice the standard error of a new observation at x:
// rmse * sqrt(1 + 1/n + (x - meanX)^2 / Sxx).
func (f fitError) band(x float64) float64 {
	if f.rmse == 0 || f.sxx == 0 {
		return 0
	}
	d := x - f.meanX
	return 2 * f.rmse * math.Sqrt(1+1/float64(f.n)+d*d/f.sxx)
}

func dayStart(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// readGrowth decodes the pack's growth section.
func readGrowth(pack *workpack.Pack) (workpack.Growth, error) {
	var g workpack.Growth
	raw := pack.File("growth.json")
	if raw == nil {
		return g, nil
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		return g, fmt.Errorf("decode growth.json: %w", err)
	}
	return g, nil
}

// growth emits the database's growth plan and each table's.
func (b *builder) growth(g workpack.Growth) {
	if p, ok := b.project(g.DBSize); ok {
		b.db = &p
		b.emitGrowth(p, KindGrowthPlan, b.databaseTarget(), "the database")
	}
	for _, t := range g.Tables {
		if p, ok := b.project(t); ok {
			b.emitGrowth(p, KindTableGrowth, "table:"+t.Table, "table "+t.Table)
		}
	}
}

// project projects s, noting why it cannot be.
func (b *builder) project(s workpack.Series) (Projection, bool) {
	if s.ID == "" {
		return Projection{}, false
	}
	p, err := Project(s, b.pack.Window.From)
	if err != nil {
		b.cov.Note("growth %s: no projection: %v", s.ID, err)
		return Projection{}, false
	}
	return p, true
}

func (b *builder) emitGrowth(p Projection, kind, target, subject string) {
	q := map[string]float64{QuantityPoints: float64(p.Points), QuantitySlope: p.SlopePerDay,
		QuantityFitRMSE: p.RMSE, QuantitySizeBytes: p.LastBytes}
	names := map[int][2]string{30: {QuantityProjected30, QuantityBand30},
		90: {QuantityProjected90, QuantityBand90}, 365: {QuantityProjected365, QuantityBand365}}
	for _, a := range p.Ahead {
		q[names[a.Days][0]], q[names[a.Days][1]] = a.Bytes, a.Band
	}
	b.emit(candidate.Candidate{Kind: kind, Target: target, EvidenceIDs: []string{p.Series},
		Reason: growthReason(p, subject), Prediction: candidate.Prediction{
			Metric: "size_bytes", Direction: direction(p.SlopePerDay),
			Source: "linear fit of the pack's daily sizes", Quantities: q}})
}

func direction(slope float64) string {
	switch {
	case slope > 0:
		return "increase"
	case slope < 0:
		return "decrease"
	}
	return "flat"
}

func growthReason(p Projection, subject string) string {
	trend := fmt.Sprintf("grows %s a day", bytesText(p.SlopePerDay))
	if p.Shrinking() {
		trend = fmt.Sprintf("is shrinking %s a day (a projection never goes below zero)",
			bytesText(-p.SlopePerDay))
	}
	s := fmt.Sprintf("%s was %s on %s and %s (%d daily points, fit error %s).", subject,
		bytesText(p.LastBytes), p.Origin.Format(time.DateOnly), trend, p.Points,
		bytesText(p.RMSE))
	for _, a := range p.Ahead {
		s += fmt.Sprintf(" In %d days: %s ± %s.", a.Days, bytesText(a.Bytes),
			bytesText(a.Band))
	}
	return s
}

// bytesText is a size for a person: GiB, MiB or bytes.
func bytesText(v float64) string {
	const mib, gib = float64(1 << 20), float64(1 << 30)
	switch a := math.Abs(v); {
	case a >= gib:
		return fmt.Sprintf("%.1f GiB", v/gib)
	case a >= mib:
		return fmt.Sprintf("%.1f MiB", v/mib)
	}
	return fmt.Sprintf("%.0f bytes", v)
}
