package agentposture

import (
	"context"
	"fmt"
	"path"
	"strings"
)

func init() { Register(ap11{}) }

// ap11 reports what would make an agent's mistake unrecoverable or
// unexplained: no point-in-time recovery (a self-managed server's WAL
// archiving, or the provider's backups as cloud telemetry last read
// them), no deletion protection on a provider instance, and no pgaudit
// where agents connect. Backups outside the blast radius (immutable or
// delete-delayed, SAFE-BAK-01) are not visible to cloud telemetry; every
// provider finding says so. All warnings.
type ap11 struct{}

func (ap11) Spec() Spec {
	return Spec{ID: "AP-11", Title: "Recovery and audit posture", Severity: Warning,
		Arms: []Arm{{Name: "archive_library", MinVersion: 150000,
			SkipReason: "archive_library exists from PostgreSQL 15; archive_command alone " +
				"decides whether WAL is archived"}}}
}

var ap11SQL = Statement("AP-11", `SELECT
  max(s.setting) FILTER (WHERE s.name = 'archive_mode'),
  max(s.setting) FILTER (WHERE s.name = 'archive_command'),
  max(s.setting) FILTER (WHERE s.name = 'archive_library'),
  max(s.setting) FILTER (WHERE s.name = 'shared_preload_libraries'),
  EXISTS (SELECT 1 FROM pg_catalog.pg_extension e WHERE e.extname = 'pgaudit')
FROM pg_catalog.pg_settings s
WHERE s.name IN ('archive_mode', 'archive_command', 'archive_library',
  'shared_preload_libraries')`)

// walArchiving is a self-managed server's archiving settings; a setting
// the role may not read is unknown.
type walArchiving struct {
	mode                       string
	command, library           string
	commandKnown, libraryKnown bool
}

// auditState is whether pgaudit is installed or preloaded.
type auditState struct {
	extension    bool
	preload      string
	preloadKnown bool
}

func (ap11) Detect(ctx context.Context, in Input) ([]Finding, error) {
	var mode, command, library, preload *string
	var a auditState
	if err := in.Q.QueryRow(ctx, ap11SQL).Scan(&mode, &command, &library, &preload,
		&a.extension); err != nil {
		return nil, fmt.Errorf("read archiving and audit settings: %w", err)
	}
	var out []Finding
	p := in.Env.Config.Platform
	if p.selfManaged() {
		w := walArchiving{mode: deref(mode), command: deref(command),
			commandKnown: command != nil, library: deref(library), libraryKnown: library != nil}
		if !in.Arm("archive_library") {
			w.library, w.libraryKnown = "", true // PG14: no such setting
		}
		if f, ok := selfManagedRecovery(w); ok {
			out = append(out, f)
		}
	} else {
		out = append(out, providerRecovery(p)...)
	}
	a.preload, a.preloadKnown = deref(preload), preload != nil
	if f, ok := pgauditFinding(in.Env, a); ok {
		out = append(out, f)
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// selfManagedRecovery reports WAL archiving that is off, or on with
// nothing that archives. Unknown settings are not judged.
func selfManagedRecovery(w walArchiving) (Finding, bool) {
	var why string
	switch {
	case w.mode == "off":
		why = "archive_mode is off: WAL is not archived"
	case (w.mode == "on" || w.mode == "always") && w.commandKnown && w.libraryKnown &&
		w.command == "" && w.library == "":
		why = "archive_mode is " + w.mode + " but no archive_command or archive_library " +
			"is set: nothing archives WAL"
	default:
		return Finding{}, false
	}
	return Finding{Severity: Warning, ObjectType: "setting", Object: "archive_mode",
		Title:  "No point-in-time recovery",
		Detail: why + ", so a bad agent write or DDL can only be undone to the last base backup.",
		Recommendation: "Archive WAL with a backup tool (pgBackRest, WAL-G, Barman) and " +
			"take base backups, so the server can be restored to just before a mistake.",
		FixScript: "ALTER SYSTEM SET archive_mode = on; -- takes effect after a restart\n" +
			"ALTER SYSTEM SET archive_command = '<your backup tool''s archive command>';\n" +
			"-- then restart, take a base backup and test a point-in-time restore",
		Caveat: "Only this server's settings are read: a tool that streams WAL " +
			"(pg_receivewal, Barman streaming) can provide PITR with archive_mode off.",
		Evidence: []Evidence{{Source: "pg_settings", Ref: "archive_mode",
			Detail: "archive_mode = " + w.mode}}}, true
}

// providerRecovery reports the provider backup posture cloud telemetry
// read: PITR off (backups or PITR disabled, retention 0) and deletion
// protection off. Unknown telemetry or fields report nothing.
func providerRecovery(p Platform) []Finding {
	b := p.Backup
	if p.selfManaged() || b == nil {
		return nil
	}
	var out []Finding
	if why := noPITR(b); why != "" {
		out = append(out, providerFinding(p, "backups", "No point-in-time recovery",
			why+": a bad agent write or DDL cannot be undone by a restore.",
			"Turn on automated backups with point-in-time recovery and a retention of "+
				"at least 7 days.", backupFix(p.Provider)))
	}
	if b.DeletionProtection != nil && !*b.DeletionProtection {
		out = append(out, providerFinding(p, "deletion_protection",
			"Deletion protection is off",
			"The instance can be deleted, with its automated backups, by one API call "+
				"from any credential allowed to.",
			"Turn on deletion protection.", deletionFix(p.Provider)))
	}
	return out
}

func noPITR(b *BackupPosture) string {
	var why []string
	if b.BackupsEnabled != nil && !*b.BackupsEnabled {
		why = append(why, "automated backups are off")
	}
	if b.PITREnabled != nil && !*b.PITREnabled {
		why = append(why, "point-in-time recovery is off")
	}
	if b.RetentionDays != nil && *b.RetentionDays == 0 {
		why = append(why, "backup retention is 0 days")
	}
	return strings.Join(why, "; ")
}

func providerFinding(p Platform, object, title, detail, rec, fix string) Finding {
	return Finding{Severity: Warning, ObjectType: "instance", Object: object, Title: title,
		Detail: detail, Recommendation: rec, FixScript: fix,
		Caveat: "From cloud telemetry's latest read of the instance. pg_sage cannot see " +
			"whether backups are immutable or kept after the instance is deleted " +
			"(SAFE-BAK-01): check the provider's backup vault or retained-backup settings.",
		Evidence: []Evidence{{Source: "cloud telemetry (" + p.Provider + ")", Ref: object,
			Detail: backupSummary(p.Backup)}}}
}

func backupSummary(b *BackupPosture) string {
	part := func(name string, v *bool) string {
		if v == nil {
			return name + " unknown"
		}
		return fmt.Sprintf("%s %t", name, *v)
	}
	ret := "retention unknown"
	if b.RetentionDays != nil {
		ret = fmt.Sprintf("retention %d days", *b.RetentionDays)
	}
	return strings.Join([]string{part("backups", b.BackupsEnabled), part("pitr",
		b.PITREnabled), ret, part("deletion_protection", b.DeletionProtection)}, ", ")
}

func backupFix(provider string) string {
	switch provider {
	case "rds":
		return "-- aws rds modify-db-instance --db-instance-identifier <id> " +
			"--backup-retention-period 7 --apply-immediately"
	case "aurora":
		return "-- aws rds modify-db-cluster --db-cluster-identifier <id> " +
			"--backup-retention-period 7 --apply-immediately"
	case "cloud-sql":
		return "-- gcloud sql instances patch <instance> --backup-start-time=03:00 " +
			"--enable-point-in-time-recovery --retained-transaction-log-days=7"
	}
	return "-- Turn on automated backups with point-in-time recovery in the provider console."
}

func deletionFix(provider string) string {
	switch provider {
	case "rds":
		return "-- aws rds modify-db-instance --db-instance-identifier <id> --deletion-protection"
	case "aurora":
		return "-- aws rds modify-db-cluster --db-cluster-identifier <id> --deletion-protection"
	case "cloud-sql":
		return "-- gcloud sql instances patch <instance> --deletion-protection"
	}
	return "-- Turn on deletion protection in the provider console."
}

// pgauditFinding reports pgaudit absent while agent roles exist. A
// preload list the role cannot read is not judged (pgaudit works without
// CREATE EXTENSION).
func pgauditFinding(env Env, a auditState) (Finding, bool) {
	if len(env.Agents) == 0 || a.extension || !a.preloadKnown ||
		preloadHasPgaudit(a.preload) {
		return Finding{}, false
	}
	return Finding{Severity: Warning, ObjectType: "extension", Object: "pgaudit",
		Title: "No pgaudit where agents connect",
		Detail: fmt.Sprintf("%d agent role(s) use this database (%s) and pgaudit is "+
			"neither preloaded nor installed: what an agent did is only in pg_sage's "+
			"own records.", len(env.Agents), roleText(env.Agents)),
		Recommendation: "Preload pgaudit and log at least writes and DDL for agent roles.",
		FixScript: "-- add pgaudit to shared_preload_libraries (keep the current entries: " +
			a.preload + "), restart, then:\nCREATE EXTENSION pgaudit;\n" +
			"-- ALTER ROLE <agent role> SET pgaudit.log = 'write, ddl';",
		Evidence: []Evidence{{Source: "pg_settings", Ref: "shared_preload_libraries",
			Detail: "'" + a.preload + "'"}}}, true
}

// preloadHasPgaudit reports whether a shared_preload_libraries value
// loads pgaudit ("pgaudit", "'pgaudit'", "$libdir/pgaudit").
func preloadHasPgaudit(preload string) bool {
	for _, lib := range strings.Split(preload, ",") {
		lib = strings.Trim(strings.TrimSpace(lib), `"'`)
		if path.Base(lib) == "pgaudit" {
			return true
		}
	}
	return false
}
