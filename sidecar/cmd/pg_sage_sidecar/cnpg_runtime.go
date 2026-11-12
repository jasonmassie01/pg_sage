package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/cnpg"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/managedparam"
	prov "github.com/pg-sage/sidecar/internal/provider"
)

// CloudNativePG (market-12). Detection sees self-managed PostgreSQL on a
// CNPG Cluster; a databases[].cnpg block (postgres.cnpg in standalone)
// names the Cluster and makes the provider cnpg, so ALTER SYSTEM is never
// used and instance settings become Cluster spec proposals. Without the
// block, CNPG evidence on the server only opens a finding.

// categoryCNPGHint is the finding that a database looks like CNPG but no
// cnpg block names its Cluster.
const categoryCNPGHint = "cnpg_target_unconfigured"

// cnpgInstanceManager is the binary CNPG's archive_command runs.
const cnpgInstanceManager = "/controller/manager"

// cnpgProvider refines the detected provider with a cnpg block: a
// self-managed (or not yet known) database becomes cnpg; a detected cloud
// is kept and the block is reported as a mistake.
func cnpgProvider(detected string, c config.CNPGConfig) (string, string) {
	if !c.Enabled() {
		return detected, ""
	}
	if detected == providerSelfManaged || detected == providerUnknown {
		return prov.CNPG, ""
	}
	return detected, fmt.Sprintf("cnpg names CloudNativePG Cluster %s/%s but the database "+
		"was detected as %s; pg_sage keeps %s and ignores the cnpg block", c.Namespace,
		c.Cluster, detected, detected)
}

// cnpgHint reads the CNPG evidence in pg_settings values: the instance
// manager in archive_command, or allow_alter_system = off (PostgreSQL 17+;
// absent before, where archive_command alone decides).
func cnpgHint(settings map[string]string) (string, bool) {
	if strings.Contains(settings["archive_command"], cnpgInstanceManager) {
		return "archive_command runs the CloudNativePG instance manager (" +
			cnpgInstanceManager + ")", true
	}
	if strings.EqualFold(strings.TrimSpace(settings["allow_alter_system"]), "off") {
		return "allow_alter_system is off, as CloudNativePG sets it", true
	}
	return "", false
}

func cnpgHintFinding(evidence string) analyzer.Finding {
	return analyzer.Finding{Category: categoryCNPGHint, Severity: "warning",
		ObjectType: "configuration", ObjectIdentifier: "instance:cnpg",
		Title:  "This database looks like a CloudNativePG Cluster that is not configured",
		Detail: map[string]any{"evidence": evidence},
		Recommendation: "pg_sage found " + evidence + ". CloudNativePG owns " +
			"postgresql.conf and disables ALTER SYSTEM, so configuration changes pg_sage " +
			"treats as self-managed would fail or be lost on the next reconcile. Name the " +
			"Cluster in the config (cnpg: {cluster: <name>, namespace: <namespace>} on the " +
			"database) so settings become Cluster spec proposals."}
}

// readCNPGHintSettings is the one bounded, read-only read of the evidence.
func readCNPGHintSettings(ctx context.Context, pool *pgxpool.Pool) (map[string]string,
	error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin CNPG evidence read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, sreBacklogLimitsSQL); err != nil {
		return nil, fmt.Errorf("bound CNPG evidence read: %w", err)
	}
	rows, err := tx.Query(ctx, `/* pg_sage */ SELECT name, setting FROM pg_settings
		WHERE name IN ('archive_command', 'allow_alter_system')`)
	if err != nil {
		return nil, fmt.Errorf("read CNPG evidence from pg_settings: %w", err)
	}
	out := map[string]string{}
	for rows.Next() {
		var name, setting string
		if err := rows.Scan(&name, &setting); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan CNPG evidence: %w", err)
		}
		out[name] = setting
	}
	rows.Close()
	return out, rows.Err()
}

// checkCNPGHint opens the hint finding on a self-managed database that
// shows CNPG evidence, and resolves it once a cnpg block names the Cluster.
func (rt *databaseRuntime) checkCNPGHint() {
	if rt.provider != providerSelfManaged && rt.provider != prov.CNPG {
		return
	}
	var findings []analyzer.Finding
	if rt.provider == providerSelfManaged {
		settings, err := readCNPGHintSettings(rt.ctx, rt.spec.Pool)
		if err != nil {
			logWarn(rt.spec.Scope, "db %q: CloudNativePG check: %v", rt.spec.Name, err)
			return
		}
		if evidence, ok := cnpgHint(settings); ok {
			findings = append(findings, cnpgHintFinding(evidence))
		}
	}
	ctx, cancel := context.WithTimeout(rt.ctx, 30*time.Second)
	defer cancel()
	if err := replaceFindings(ctx, rt.spec.Pool, categoryCNPGHint, findings); err != nil {
		logWarn(rt.spec.Scope, "db %q: CloudNativePG finding: %v", rt.spec.Name, err)
	}
}

// cnpgShareKey is the peer resource of every database on one Cluster.
func cnpgShareKey(c config.CNPGConfig) string {
	return "cnpg|cluster=" + c.Namespace + "/" + c.Cluster
}

// cnpgResolver reads the named Cluster with the pod's service account.
func cnpgResolver(c config.CNPGConfig) (managedparam.Resolver, error) {
	src, err := cnpg.NewSource(cnpg.Cluster{Name: c.Cluster, Namespace: c.Namespace})
	if err != nil {
		return nil, err
	}
	return src, nil
}

// cnpgChangeResolver is the managed-change resolver of a cnpg database (nil
// with a warning when the Cluster cannot be read: proposals then use
// placeholders).
func (rt *databaseRuntime) cnpgChangeResolver() managedparam.Resolver {
	r, err := cnpgResolver(rt.spec.Config.CNPG)
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: %v", rt.spec.Name, err)
		return nil
	}
	return r
}
