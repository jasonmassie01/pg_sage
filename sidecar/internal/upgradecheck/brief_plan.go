package upgradecheck

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/pgversion"
)

// hoursPerMonth is the billing month of AWS and Azure (730 h).
const hoursPerMonth = 730.0

// maxNamed bounds the objects a checklist step names.
const maxNamed = 10

// extendedSupportProviders bill extended support once a major leaves
// standard support (R4 §2.1).
var extendedSupportProviders = map[string]bool{pgversion.RDS: true,
	pgversion.Aurora: true, providerAzure: true}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// costOfWaiting prices the wait at the operator's rate only; the DBA
// hours are always stated, as an estimate with their formula.
func costOfWaiting(b Brief, rate *ExtendedSupportRate) CostOfWaiting {
	out := CostOfWaiting{DBAHours: dbaHours(b)}
	if !extendedSupportProviders[b.Provider] {
		out.Status = CostNotApplicable
		out.Reason = fmt.Sprintf("provider %q bills no extended support; past its end of "+
			"support a major gets no fixes", b.Provider)
		return out
	}
	switch {
	case rate == nil:
		out.Status, out.Reason = CostUnpriced, "set cost.rates[].extended_support_per_"+
			"vcpu_hour and vcpus for this database to price the wait"
		return out
	case rate.VCPUs <= 0:
		out.Status, out.Reason = CostUnpriced, "the rate has no vcpus: set "+
			"cost.rates[].vcpus to price the wait"
		return out
	}
	out.Status, out.Currency, out.RateAsOf = CostPriced, rate.Currency, rate.AsOf
	out.PerMonth = round2(rate.PerVCPUHour * float64(rate.VCPUs) * hoursPerMonth)
	out.Formula = fmt.Sprintf("%g per vCPU-hour x %d vCPUs x %g h/month = %.2f %s per "+
		"month; accrued = months in extended support x per month", rate.PerVCPUHour,
		rate.VCPUs, hoursPerMonth, out.PerMonth, rate.Currency)
	w, ok := pgversion.Support(b.Provider, b.Source.Major)
	end := w.EndTime()
	if !ok || end.IsZero() {
		out.Reason = fmt.Sprintf("%s's standard-support end for PostgreSQL %d is not in "+
			"pg_sage's dated table: months unknown", b.Provider, b.Source.Major)
		return out
	}
	from := end.AddDate(0, 0, 1)
	out.ExtendedSupportFrom = from.Format(time.DateOnly)
	out.Months = wholeMonths(from, b.BuiltAt)
	out.Accrued = round2(float64(out.Months) * out.PerMonth)
	out.Reason = fmt.Sprintf("PostgreSQL %d is billed extended support on %s from %s",
		b.Source.Major, b.Provider, out.ExtendedSupportFrom)
	return out
}

// wholeMonths is the whole months from from to now, 0 before from.
func wholeMonths(from, now time.Time) int {
	m := (now.Year()-from.Year())*12 + int(now.Month()-from.Month())
	if now.Day() < from.Day() {
		m--
	}
	return max(m, 0)
}

func dbaHours(b Brief) HoursEstimate {
	ext := 0
	for _, e := range b.ExtensionPath.Items {
		if !strings.HasPrefix(e.Path, "carried") {
			ext++
		}
	}
	low := 4 + 2*float64(len(b.Blockers.Items)) + float64(b.SlotPlan.Total) +
		0.5*float64(ext) + 0.1*float64(b.CollationReindex.Total)
	low = math.Round(low*10) / 10
	return HoursEstimate{Low: low, High: 2 * low, Formula: "estimate: low = 4 h + 2 h per " +
		"blocker + 1 h per slot or subscription + 0.5 h per extension to act on + " +
		"0.1 h per index to reindex; high = 2 x low (rehearsal reruns)"}
}

// named joins up to maxNamed names, then counts the rest.
func named(names []string) string {
	if len(names) > maxNamed {
		return strings.Join(names[:maxNamed], "; ") + "; and " +
			strconv.Itoa(len(names)-maxNamed) + " more"
	}
	return strings.Join(names, "; ")
}

// sectionDetail is a step's detail: unknown with its reason, none, or the
// names.
func sectionDetail(s Section, names []string, none string) string {
	switch {
	case s.State == SectionUnknown && len(names) > 0:
		return "unknown: " + s.Reason + " (" + named(names) + ")"
	case s.State == SectionUnknown:
		return "unknown: " + s.Reason
	case len(names) == 0:
		return none
	}
	return named(names)
}

// checklist is the ordered plan: blockers, extensions, reindex, slots,
// rehearsal, the operator's cutover, then the post-upgrade steps.
func checklist(b Brief) []ChecklistStep {
	steps := []ChecklistStep{
		{Step: "resolve_blockers", Title: "Resolve the blockers", Owner: "operator",
			Detail: blockerDetail(b.Blockers)},
		{Step: "extension_path", Title: "Prepare each extension's path", Owner: "operator",
			Detail: sectionDetail(b.ExtensionPath.Section, extensionNames(b),
				"no extensions beyond plpgsql")},
		{Step: "reindex_plan", Title: "Plan the collation reindex", Owner: "pg_sage",
			Detail: sectionDetail(b.CollationReindex.Section, reindexNames(b),
				"no indexes to reindex")},
		{Step: "slot_plan", Title: "Plan each logical slot and subscription",
			Owner: "operator", Detail: sectionDetail(b.SlotPlan.Section, slotNames(b),
				"no logical slots or subscriptions")},
		{Step: "rehearsal", Title: "Rehearse on a clone", Owner: "operator",
			Detail: "upgrade a clone, ANALYZE it and compare the top queries' plans " +
				"before and after (drill-02b)"},
		{Step: "cutover", Title: "Cut over", Owner: "operator",
			Detail: fmt.Sprintf("the upgrade itself stays with the operator: run %s to "+
				"PostgreSQL %d in a window; pg_sage does not run it", b.Method,
				b.Target.Major)},
		{Step: "post_upgrade", Title: "After the upgrade", Owner: "pg_sage",
			Detail: postUpgradeDetail(b)},
	}
	for i := range steps {
		steps[i].Order = i + 1
	}
	return steps
}

func blockerDetail(s BlockerSection) string {
	parts := make([]string, 0, len(s.Items))
	for _, bl := range s.Items {
		parts = append(parts, bl.Check+": "+bl.Title+" ("+strconv.Itoa(bl.Total)+")")
	}
	out := NoBlockersFound
	if len(parts) > 0 {
		out = named(parts)
	}
	if len(s.Unknown) > 0 {
		out += "; could not tell: " + strings.Join(s.Unknown, ", ")
	}
	return out
}

func extensionNames(b Brief) []string {
	var out []string
	for _, e := range b.ExtensionPath.Items {
		out = append(out, e.Extension+" "+e.InstalledVersion+": "+e.Path)
	}
	return out
}

func reindexNames(b Brief) []string {
	var out []string
	for _, r := range b.CollationReindex.Items {
		out = append(out, r.Action+" "+r.Index)
	}
	return out
}

func slotNames(b Brief) []string {
	var out []string
	for _, s := range b.SlotPlan.Items {
		out = append(out, s.Kind+" "+s.Name+" ("+s.Consumer+"): "+s.Step)
	}
	return out
}

func postUpgradeDetail(b Brief) string {
	stats := "ANALYZE every database: statistics are not carried before PostgreSQL 18"
	if b.Target.Major >= 18 && b.Method == PGUpgrade {
		stats = "ANALYZE every database (pg_upgrade to 18 carries most statistics, not " +
			"extended statistics)"
	}
	return stats + "; run the reindex steps; ALTER EXTENSION ... UPDATE where the path " +
		"says so; REFRESH COLLATION VERSION once rebuilt (drill-02c)"
}

// briefHash is the sha256 of the brief's content without its build time.
func briefHash(b Brief) string {
	b.BuiltAt, b.Hash = time.Time{}, ""
	doc, err := json.Marshal(b)
	if err != nil { // a Brief is plain data; Marshal cannot fail on it
		return ""
	}
	sum := sha256.Sum256(doc)
	return hex.EncodeToString(sum[:])
}
