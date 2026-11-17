package upgradecheck

import (
	"errors"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// providerAzure is the cloud_environment value of Azure Database for
// PostgreSQL, whose in-place upgrade precheck refuses hypopg (R4 §2.1).
const providerAzure = "azure"

// newBrief composes the brief from one run and the facts read with it.
func newBrief(rep Report, f briefFacts, opts BriefOptions) Brief {
	now := rep.CheckedAt
	provider := rep.Target.Provider
	b := Brief{Database: rep.Database, Method: rep.Target.Method, Provider: provider,
		TargetDatabase: rep.TargetDatabase, BuiltAt: now,
		Source: BriefVersion{Major: rep.SourceMajor, VersionNum: rep.SourceVersionNum,
			Support: supportState(provider, rep.SourceMajor, now)},
		Target: BriefVersion{Major: rep.Target.Major,
			Support: supportState(provider, rep.Target.Major, now)}}
	b.ExtensionPath = extensionSection(rep, f)
	b.Blockers = blockerSection(rep, b.ExtensionPath)
	b.SlotPlan = slotSection(rep, opts)
	b.CollationReindex = reindexSection(rep, f)
	b.CostOfWaiting = costOfWaiting(b, opts.Rate)
	b.Checklist = checklist(b)
	b.Hash = briefHash(b)
	return b
}

// sqlState is err's SQLSTATE, or "".
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func unknownSection(why string, err error) Section {
	s := Section{State: SectionUnknown, Reason: why}
	if err != nil {
		s.Reason += ": " + err.Error()
		s.SQLState = sqlState(err)
	}
	return s
}

func blockerSection(rep Report, ext ExtensionSection) BlockerSection {
	out := BlockerSection{Section: Section{State: SectionKnown}, Items: []Blocker{}}
	for _, c := range rep.Checks {
		switch c.Status {
		case Block:
			out.Items = append(out.Items, Blocker{Check: c.ID, Title: c.Title,
				Reason: c.Reason, Total: c.Total})
		case Unknown:
			out.Unknown = append(out.Unknown, c.ID)
		}
	}
	if hypoPGBlocksInPlace(rep, ext) {
		out.Items = append(out.Items, Blocker{Check: "U2",
			Title: "hypopg blocks Azure's in-place upgrade", Total: 1,
			Reason: "Azure's major-version upgrade precheck refuses hypopg: DROP " +
				"EXTENSION hypopg before the upgrade and CREATE EXTENSION hypopg after " +
				"(it holds no data)"})
	}
	out.Summary = NoBlockersFound
	out.Reason = "every check that blocks the upgrade, from this run"
	if n := len(out.Items); n > 0 {
		out.Summary = strconv.Itoa(n) + " blockers"
	}
	if len(out.Unknown) > 0 {
		out.Reason += "; these checks could not tell: " + strings.Join(out.Unknown, ", ")
	}
	return out
}

func hypoPGBlocksInPlace(rep Report, ext ExtensionSection) bool {
	if rep.Target.Provider != providerAzure || rep.Target.Method != PGUpgrade {
		return false
	}
	return slices.ContainsFunc(ext.Items, func(e ExtensionStep) bool {
		return e.Extension == "hypopg"
	})
}

func extensionSection(rep Report, f briefFacts) ExtensionSection {
	out := ExtensionSection{Items: []ExtensionStep{}}
	switch {
	case f.installedErr != nil:
		out.Section = unknownSection("the installed extensions could not be read",
			f.installedErr)
		return out
	case len(f.installed) == 0:
		out.Section = Section{State: SectionKnown,
			Reason: "no extensions beyond plpgsql are installed"}
		return out
	case f.available == nil:
		out.Section = Section{State: SectionUnknown, Reason: f.availableWhy}
		for _, e := range f.installed {
			out.Items = append(out.Items, ExtensionStep{Extension: e.name,
				InstalledVersion: e.version, Path: "unknown"})
		}
		return out
	}
	out.Section = Section{State: SectionKnown, Reason: "read from target_database " +
		rep.TargetDatabase}
	for _, e := range f.installed {
		out.Items = append(out.Items, extensionStep(e, f.available[e.name], rep))
	}
	return out
}

func extensionStep(e extension, versions []string, rep Report) ExtensionStep {
	step := ExtensionStep{Extension: e.name, InstalledVersion: e.version,
		TargetVersions: append([]string{}, versions...)}
	switch {
	case len(versions) == 0:
		step.Blocker = true
		step.Path = "not offered by the target: install it there first, or drop it"
	case slices.Contains(versions, e.version):
		step.Path = "carried: the target offers " + e.version
	default:
		step.Path = "update after the upgrade: ALTER EXTENSION " + e.name +
			" UPDATE (the target offers " + strings.Join(versions, ", ") + ")"
	}
	if e.name == "hypopg" && rep.Target.Provider == providerAzure &&
		rep.Target.Method == PGUpgrade {
		step.Blocker = true
		step.Path = "drop before Azure's in-place upgrade and re-create after"
	}
	return step
}

func slotSection(rep Report, opts BriefOptions) SlotSection {
	out := SlotSection{Items: []SlotStep{}}
	u3, ok := rep.Check("U3")
	if !ok || u3.Status == Unknown {
		out.Section = Section{State: SectionUnknown, Reason: u3.Reason, SQLState: u3.SQLState}
		return out
	}
	out.Section = Section{State: SectionKnown, Reason: u3.Reason}
	out.Total = u3.Total
	for _, r := range u3.Rows {
		s := SlotStep{Kind: r["kind"], Plugin: r["plugin"]}
		if s.Kind == "subscription" {
			s.Name, s.Consumer = r["subscription"], "this database (subscriber)"
		} else {
			s.Name, s.Consumer = r["slot"], slotConsumer(r["slot"], opts)
		}
		s.Step = slotPlanStep(rep, s.Kind)
		out.Items = append(out.Items, s)
	}
	return out
}

// slotConsumer is the confirmed slot_consumer fact's consumer of slot.
func slotConsumer(slot string, opts BriefOptions) string {
	if opts.SlotConsumersErr != nil {
		return "unknown: the slot_consumer facts could not be read: " +
			opts.SlotConsumersErr.Error()
	}
	if c, ok := opts.SlotConsumers[slot]; ok {
		return c
	}
	subjects := make([]string, 0, len(opts.SlotConsumers))
	for s := range opts.SlotConsumers {
		subjects = append(subjects, s)
	}
	slices.Sort(subjects) // the first matching subject, deterministically
	for _, s := range subjects {
		if ok, err := path.Match(s, slot); err == nil && ok {
			return opts.SlotConsumers[s]
		}
	}
	return "unknown: no confirmed slot_consumer fact names it"
}

func slotPlanStep(rep Report, kind string) string {
	src := "PostgreSQL " + strconv.Itoa(rep.SourceMajor)
	switch {
	case rep.Target.Method != PGUpgrade && kind == "subscription":
		return "repoint the subscription at the new primary at switchover"
	case rep.Target.Method != PGUpgrade:
		return "recreate the slot on the new primary at switchover and repoint the " +
			"consumer (it may re-snapshot)"
	case rep.SourceMajor >= 17 && kind == "subscription":
		return "carried by pg_upgrade from " + src + ": check it is enabled after"
	case rep.SourceMajor >= 17:
		return "carried by pg_upgrade from " + src + " once it has consumed all WAL " +
			"(the new cluster needs wal_level=logical): check the consumer resumes"
	case kind == "subscription":
		return "refresh or recreate the subscription after the upgrade: pg_upgrade from " +
			src + " does not keep its table sync state"
	}
	return "drop and recreate after, consumer re-snapshots: drain the consumer, drop " +
		"the slot before pg_upgrade (it does not carry slots from " + src + ") and " +
		"recreate it after; or use blue/green to keep the consumer streaming"
}

func reindexSection(rep Report, f briefFacts) ReindexSection {
	out := ReindexSection{Items: []ReindexStep{}}
	u5, _ := rep.Check("U5")
	switch {
	case f.reindexErr != nil:
		out.Section = unknownSection("the indexes on changed collations could not be "+
			"read", f.reindexErr)
		return out
	case u5.Status == Unknown:
		out.Section = Section{State: SectionUnknown, Reason: u5.Reason,
			SQLState: u5.SQLState}
		return out
	}
	out.Section = Section{State: SectionKnown, Reason: "indexes on collations whose " +
		"version changed, and indexes the target folds case differently for; a " +
		"collation the target's operating system changes is not seen from here"}
	out.Skipped = u5.Skipped
	out.Items = append(out.Items, f.reindex...)
	out.Total = f.reindexTotal
	listed := map[string]bool{}
	for _, r := range f.reindex {
		listed[r.Index] = true
	}
	for _, r := range u5.Rows {
		if r["kind"] != "index" || listed[r["name"]] { // one rebuild covers both reasons
			continue
		}
		out.Total++
		if len(out.Items) < MaxRows {
			out.Items = append(out.Items, ReindexStep{Action: ReindexAction,
				Index: r["name"], Reason: "PostgreSQL 18 folds case by the database's " +
					"provider for full text search and pg_trgm"})
		}
	}
	return out
}
