package connections

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// view is what the generator reads from the pack: whether its connections
// section holds the hours the candidates cite, the header's timezone, and
// the usable connections and peak client backends when both are measured.
type view struct {
	hasHours     bool
	loc          *time.Location
	measured     bool
	usable, peak int
	maxConnID    string
}

func readView(pack *workpack.Pack) (view, error) {
	v := view{loc: time.UTC}
	if pack == nil {
		return v, fmt.Errorf("connections generator: no evidence pack")
	}
	if raw := pack.File("connections.json"); raw != nil {
		var c workpack.Connections
		if err := json.Unmarshal(raw, &c); err != nil {
			return v, fmt.Errorf("decode connections.json: %w", err)
		}
		v.hasHours = len(c.Hours) > 0
	}
	h, err := pack.Header()
	if err != nil {
		return v, err
	}
	if h.Timezone != "" {
		if loc, err := time.LoadLocation(h.Timezone); err == nil {
			v.loc = loc
		}
	}
	s, ok, err := readSettings(pack)
	if err != nil || !ok {
		return v, err
	}
	usable, uok := s.Measures.UsableConnections.(float64)
	peak, pok := s.Measures.PeakClientBackends.(float64)
	if uok && pok && usable > 0 && peak >= 0 {
		v.measured, v.usable, v.peak = true, int(usable), int(peak)
	}
	for _, f := range s.Settings {
		if f.Name == "max_connections" {
			v.maxConnID = f.ID
		}
	}
	return v, nil
}

// headroomPct is the free share of usable connections at peak, percent.
func (v view) headroomPct() float64 {
	return float64(v.usable-v.peak) * 100 / float64(v.usable)
}

// hour is the connections fact of the hour t fell in.
func (v view) hour(t time.Time) string {
	return "connections/hour/" + strconv.Itoa(hourOfWeek(t, v.loc))
}

// hours is the sorted, distinct connection facts of times.
func (v view) hours(times []time.Time) []string {
	var out []string
	for _, t := range times {
		if id := v.hour(t); !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// builder collects the candidates of one Build.
type builder struct {
	v   view
	in  Inputs
	cov *candidate.Coverage
	out []candidate.Candidate
}

// emit stamps the generator's fields and the id. Every candidate needs a
// person's approval: a review has no budget's shadow data to earn more.
func (b *builder) emit(c candidate.Candidate) {
	c.Generator, c.Dimension = Name, Dimension
	c.Approval = candidate.ApprovalRequired
	c.ID = candidate.NewID(Name, c.Kind, c.Target, c.IDBody())
	if c.Prediction.Tier == "" {
		c.Prediction.Tier = candidate.TierNone
	}
	b.out = append(b.out, c)
}

// appTarget and roleTarget name a candidate's subject.
func appTarget(app string) string   { return "application:" + app }
func roleTarget(role string) string { return "role:" + role }

// appName is how a reason names an application.
func appName(app string) string {
	if app == "" {
		return "sessions with no application_name"
	}
	return "application " + strconv.Quote(app)
}
