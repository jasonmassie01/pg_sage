package discovery

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// First grade and baseline (fleetpol-01b): once enrolled, a member's
// first review-01 grade is asked for (review-04a's enrollment trigger),
// and the runner records when it was first graded and first met its
// set's grade requirements. Reading grades never touches the member's
// workload: grades live in the member's own sage schema.

// Grade is one finished review-01 grade of a member: when, and the letter
// per rubric dimension.
type Grade struct {
	At      time.Time
	Letters map[string]string
}

// Grades asks for and reads members' grades. RequestFirstGrade may be
// called again for the same member (it must be idempotent); an error
// wrapping ErrMemberNotReady is retried next cycle without a report.
type Grades interface {
	RequestFirstGrade(ctx context.Context, member string, row store.DiscoveredRow) error
	// Grades is the member's finished grades, oldest first.
	Grades(ctx context.Context, member string) ([]Grade, error)
}

// grade advances every enrolled member that has not reached its set's
// baseline yet.
func (r *Runner) grade(ctx context.Context, sets []policy.PolicySet, now time.Time,
	rep *Report) {
	if r.grades == nil {
		return
	}
	rows, err := r.store.PendingGrades(ctx)
	if err != nil {
		rep.Errors = append(rep.Errors, err)
		return
	}
	bySet := make(map[string]policy.PolicySet, len(sets))
	for _, s := range sets {
		bySet[s.Name] = s
	}
	for _, row := range rows {
		r.gradeRow(ctx, row, bySet[row.SetName], now, rep)
	}
}

func (r *Runner) gradeRow(ctx context.Context, row store.DiscoveredRow, set policy.PolicySet,
	now time.Time, rep *Report) {
	member := MemberName(row)
	r.checkDeadline(row, set, now, rep)
	if row.GradeRequestedAt == nil {
		err := r.grades.RequestFirstGrade(ctx, member, row)
		if errors.Is(err, ErrMemberNotReady) {
			return
		}
		if err == nil {
			err = r.store.MarkMilestone(ctx, row.ID, store.MilestoneGradeRequested, now)
		}
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Errorf("asking for %s's first grade: %w",
				member, err))
			return
		}
	}
	grades, err := r.grades.Grades(ctx, member)
	if errors.Is(err, ErrMemberNotReady) {
		return
	}
	if err != nil {
		rep.Errors = append(rep.Errors, fmt.Errorf("reading %s's grades: %w", member, err))
		return
	}
	r.recordGrades(ctx, row, set, grades, rep)
}

func (r *Runner) recordGrades(ctx context.Context, row store.DiscoveredRow,
	set policy.PolicySet, grades []Grade, rep *Report) {
	if len(grades) == 0 {
		return
	}
	if row.FirstGradedAt == nil {
		if err := r.store.MarkMilestone(ctx, row.ID, store.MilestoneFirstGraded,
			grades[0].At); err != nil {
			rep.Errors = append(rep.Errors, err)
			return
		}
		rep.Graded++
	}
	for _, g := range grades {
		if !policy.GradesMet(set.Requirements.Grades, g.Letters) {
			continue
		}
		if err := r.store.MarkMilestone(ctx, row.ID, store.MilestoneBaseline, g.At); err != nil {
			rep.Errors = append(rep.Errors, err)
			return
		}
		rep.AtBaseline++
		return
	}
}

// checkDeadline reports, once, a member not graded within its set's
// enrollment deadline (enrollment_deadline_hours, else the shortest
// grade requirement's within_hours).
func (r *Runner) checkDeadline(row store.DiscoveredRow, set policy.PolicySet,
	now time.Time, rep *Report) {
	hours := set.Requirements.EnrollmentDeadlineHours
	for _, g := range set.Requirements.Grades {
		if hours == 0 || g.WithinHours < hours {
			hours = g.WithinHours
		}
	}
	if hours <= 0 || row.FirstGradedAt != nil || row.EnrolledAt == nil ||
		!now.After(row.EnrolledAt.Add(time.Duration(hours)*time.Hour)) {
		return
	}
	key := "overdue\x00" + strconv.FormatInt(row.ID, 10)
	if r.warned[key] {
		return
	}
	r.warned[key] = true
	rep.Overdue = append(rep.Overdue, fmt.Sprintf("%s/%s (%s): no first grade within %dh "+
		"of enrollment, the %s set's deadline", row.Source, row.ExternalID, MemberName(row),
		hours, set.Name))
}

// refreshStats keeps the per-set numbers WriteMetrics exports.
func (r *Runner) refreshStats(ctx context.Context, now time.Time, rep *Report) {
	stats, err := r.store.EnrollStats(ctx, now)
	if err != nil {
		rep.Errors = append(rep.Errors, err)
		return
	}
	r.statsMu.Lock()
	r.stats = stats
	r.statsMu.Unlock()
}

const (
	enrollSecondsMetric = "pg_sage_fleet_enroll_seconds"
	baselineRatioMetric = "pg_sage_fleet_baseline_24h_ratio"
)

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// WriteMetrics exports the last cycle's per-set enrollment numbers; it
// writes nothing before the first cycle with policy sets.
func (r *Runner) WriteMetrics(b *strings.Builder) {
	r.statsMu.Lock()
	stats := append([]store.EnrollStat(nil), r.stats...)
	r.statsMu.Unlock()
	if len(stats) == 0 {
		return
	}
	metricsreg.Header(b, enrollSecondsMetric)
	for _, s := range stats {
		set := labelEscaper.Replace(s.Set)
		fmt.Fprintf(b, "%s{set=\"%s\",quantile=\"0.5\"} %s\n", enrollSecondsMetric, set,
			formatFloat(s.P50Seconds))
		fmt.Fprintf(b, "%s{set=\"%s\",quantile=\"0.95\"} %s\n", enrollSecondsMetric, set,
			formatFloat(s.P95Seconds))
	}
	b.WriteString("\n")
	metricsreg.Header(b, baselineRatioMetric)
	for _, s := range stats {
		if s.Eligible > 0 {
			fmt.Fprintf(b, "%s{set=\"%s\"} %s\n", baselineRatioMetric,
				labelEscaper.Replace(s.Set),
				formatFloat(float64(s.AtBaseline)/float64(s.Eligible)))
		}
	}
	b.WriteString("\n")
}

func formatFloat(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
