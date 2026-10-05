package tuning

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/advisor"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/managedparam"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/verify"
)

// Configuration proposals go through the same gates as every other
// configuration change (advisor.GateConfigFindings): documented safe
// ranges, the allowlists, operator approval for restart-required
// settings, shared_buffers grounded in host memory, and managed-service
// forms.

// outOfScopeGUCs are instance-capacity settings the configuration advisor
// owns (WAL, checkpoints, connections): no workload case justifies them.
var outOfScopeGUCs = map[string]bool{"max_wal_size": true, "min_wal_size": true,
	"wal_buffers": true, "checkpoint_timeout": true, "checkpoint_completion_target": true,
	"max_connections": true}

// plainValue is a setting value pg_sage will quote into SQL.
var plainValue = regexp.MustCompile(`^[A-Za-z0-9_.+-]{1,32}$`)

// judgeGUC admits a server setting change.
func (v *validator) judgeGUC(c Case, p Proposal) Judged {
	name, value := strings.ToLower(strings.TrimSpace(p.Name)), strings.TrimSpace(p.Value)
	switch {
	case !pgconf.AdvisorGUC(name):
		return reject(p, ReasonInvalid, "%q is not a setting pg_sage may change", p.Name)
	case outOfScopeGUCs[name] || strings.HasPrefix(name, "checkpoint_"):
		return reject(p, ReasonOutOfScope, "%s is instance capacity, left to the "+
			"configuration advisor", name)
	case !plainValue.MatchString(value):
		return reject(p, ReasonInvalid, "value %q is not a plain setting value", p.Value)
	}
	if ok, why := pgconf.ValidateValue(name, value); !ok {
		return reject(p, ReasonInvalid, "%s", why)
	}
	metric, fall := gucMetric(name)
	if _, why := predictedChange(p, fall); why != "" {
		return reject(p, ReasonInvalid, "%s", why)
	}
	spills, bad := v.windowEvidence(c, p, metric == "temp_spills")
	if bad != nil {
		return *bad
	}
	rej, approval, hist := v.historyCheck(p, name, currentSetting(v.cur, name), value,
		gucParser(name))
	if rej != nil {
		return *rej
	}
	category := "memory_tuning"
	if strings.HasPrefix(name, "autovacuum") {
		category = "vacuum_tuning"
	}
	f := analyzer.Finding{Category: category, Severity: "info", ObjectType: "configuration",
		ObjectIdentifier: "instance:" + name,
		Title:            fmt.Sprintf("Set %s to %s", name, value),
		Detail: map[string]any{"setting": name, "value": value,
			"current": currentSetting(v.cur, name), "action_risk": "moderate"},
		Recommendation: p.Rationale,
		RecommendedSQL: fmt.Sprintf("ALTER SYSTEM SET %s = '%s'", name, value),
		RollbackSQL:    gucRollback(v.cur, name), ActionRisk: "moderate"}
	if metric == "temp_spills" {
		f.Detail["temp_blks_in_window"] = spills
	}
	return v.applyHistory(v.gated(c, p, f, verify.ClassGUC, metric), approval, hist)
}

// windowEvidence refuses a configuration change without counters measured
// over the interval; for a spill setting it also needs temp blocks the
// case statements wrote in it (cumulative totals are never evidence). It
// returns those blocks.
func (v *validator) windowEvidence(c Case, p Proposal, spill bool) (int64, *Judged) {
	if v.window.from.IsZero() {
		j := reject(p, ReasonNoRecentEvidence, "no earlier sample: the counters are "+
			"cumulative since they were first tracked, not a rate over a window")
		return 0, &j
	}
	if !spill {
		return 0, nil
	}
	var blocks int64
	for _, s := range c.Statements {
		if s.Windowed {
			blocks += s.TempBlksWritten
		}
	}
	if blocks == 0 {
		j := reject(p, ReasonNoRecentEvidence, "no temp spills measured from %s to %s",
			v.window.from.UTC().Format(time.RFC3339), v.window.to.UTC().Format(time.RFC3339))
		return 0, &j
	}
	return blocks, nil
}

// gucMetric is the metric a setting is judged on and whether it should
// fall.
func gucMetric(name string) (string, bool) {
	switch {
	case name == "work_mem" || name == "hash_mem_multiplier":
		return "temp_spills", true
	case strings.HasPrefix(name, "autovacuum"):
		return verify.MetricDeadTuples, true
	}
	return verify.MetricMeanExecTime, true
}

// gated passes a configuration finding through the shared gates and
// builds the judged result.
func (v *validator) gated(c Case, p Proposal, f analyzer.Finding, class,
	metric string) Judged {
	var settings []collector.PGSetting
	if v.cur.ConfigData != nil {
		settings = v.cur.ConfigData.PGSettings
	}
	s := v.a.settings
	out := advisor.GateConfigFindingsHost([]analyzer.Finding{f}, s.hostMemory(), s.CloudEnv,
		s.DatabaseName, settings)
	if len(out) != 1 {
		return reject(p, ReasonInvalid, "the configuration gates refused %s",
			f.RecommendedSQL)
	}
	verdict := VerdictAdmitted
	if strings.TrimSpace(out[0].RecommendedSQL) == "" {
		// A managed service's parameter group / flag change is redirected
		// to the provider for an operator; anything else is refused.
		if _, managed := managedparam.IntentFromDetail(out[0].Detail); !managed {
			return reject(p, ReasonUnavailable, "not executable here: %s",
				strings.TrimSpace(out[0].Recommendation))
		}
		verdict = VerdictRedirected
	}
	g := out[0]
	pred := verify.Prediction{Class: class, Method: verify.MethodModel, Metric: metric,
		ExpectedChangePct: p.ExpectedChangePct, TargetQueryIDs: v.targets(c, p),
		Source: PredictionSource, Note: "the model's estimate"}
	return Judged{Proposal: p, Verdict: verdict, Finding: &g, Class: class,
		Prediction: pred, Tables: c.Tables}
}

func currentSetting(snap *collector.Snapshot, name string) string {
	if s, ok := findSetting(snap, name); ok {
		return s.Setting + s.Unit
	}
	return ""
}

func findSetting(snap *collector.Snapshot, name string) (collector.PGSetting, bool) {
	if snap == nil || snap.ConfigData == nil {
		return collector.PGSetting{}, false
	}
	for _, s := range snap.ConfigData.PGSettings {
		if s.Name == name {
			return s, true
		}
	}
	return collector.PGSetting{}, false
}

// gucRollback restores the setting: RESET when it is at its default,
// else its current value in the base unit. The executor captures the
// prior value again before it runs the change.
func gucRollback(snap *collector.Snapshot, name string) string {
	s, ok := findSetting(snap, name)
	if !ok || s.Source == "default" || !plainValue.MatchString(s.Setting) {
		return "ALTER SYSTEM RESET " + name
	}
	return fmt.Sprintf("ALTER SYSTEM SET %s = '%s'", name, s.Setting)
}

// judgeReloption admits a table storage parameter change.
func (v *validator) judgeReloption(c Case, p Proposal) Judged {
	table, j, ok := v.tableInCase(c, p, p.Table)
	if !ok {
		return j
	}
	opt, value := strings.ToLower(strings.TrimSpace(p.Option)), strings.TrimSpace(p.Value)
	if !pgconf.AdvisorReloption(opt) {
		return reject(p, ReasonInvalid, "%q is not a storage parameter pg_sage may set",
			p.Option)
	}
	if !plainValue.MatchString(value) {
		return reject(p, ReasonInvalid, "value %q is not a plain value", p.Value)
	}
	if ok, why := pgconf.ValidateReloption(opt, value); !ok {
		return reject(p, ReasonInvalid, "%s", why)
	}
	metric, category, fall := verify.MetricDeadTuples, "vacuum_tuning", true
	if opt == "fillfactor" {
		metric, category, fall = "hot_updates", "table_tuning", false
	}
	if _, why := predictedChange(p, fall); why != "" {
		return reject(p, ReasonInvalid, "%s", why)
	}
	if _, bad := v.windowEvidence(c, p, false); bad != nil {
		return *bad
	}
	key := table + "|" + opt
	rej, approval, hist := v.historyCheck(p, key, v.currentReloption(key, table, opt),
		value, plainNumber)
	if rej != nil {
		return *rej
	}
	f := analyzer.Finding{Category: category, Severity: "info", ObjectType: "table",
		ObjectIdentifier: table + ":" + opt,
		Title:            fmt.Sprintf("Set %s = %s on %s", opt, value, table),
		Detail: map[string]any{"table": table, "option": opt, "value": value,
			"action_risk": "moderate"},
		Recommendation: p.Rationale,
		RecommendedSQL: fmt.Sprintf("ALTER TABLE %s SET (%s = %s)", table, opt, value),
		RollbackSQL:    fmt.Sprintf("ALTER TABLE %s RESET (%s)", table, opt),
		ActionRisk:     "moderate"}
	j = v.applyHistory(v.gated(c, p, f, verify.ClassReloption, metric), approval, hist)
	j.Tables = []string{table}
	return j
}

// currentReloption is a table's current value of opt: the last recorded
// change, else the snapshot's storage parameters, else "".
func (v *validator) currentReloption(key, table, opt string) string {
	if acts := v.history[key]; len(acts) > 0 {
		return acts[len(acts)-1].To
	}
	ts, ok := findSnapshotTable(v.cur, table)
	if !ok {
		return ""
	}
	raw := strings.Trim(reloptions(v.cur, ts), "{}")
	for _, kv := range strings.Split(raw, ",") {
		if k, val, ok := strings.Cut(strings.TrimSpace(kv), "="); ok &&
			strings.EqualFold(k, opt) {
			return val
		}
	}
	return ""
}
