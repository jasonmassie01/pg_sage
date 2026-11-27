package managedparam

import (
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/pg-sage/sidecar/internal/pgconf"
)

// A CloudNativePG Cluster (market-12) carries its settings in
// spec.postgresql.parameters; the operator writes postgresql.conf from it
// and disables ALTER SYSTEM. The proposal is a kubectl merge patch of that
// one key, the patch that restores it, and the same change as a GitOps
// fragment. The value stays in PostgreSQL's form ("64MB"), as operators
// write it in the manifest.

const (
	placeholderCluster   = "<cluster>"
	placeholderNamespace = "<namespace>"
)

// rfc1123Label is a Kubernetes object or namespace name segment.
var rfc1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// cnpgFixedParameters are the settings the operator owns: its webhook
// refuses them in spec.postgresql.parameters, or another Cluster field sets
// them (shared_preload_libraries, synchronous_standby_names).
var cnpgFixedParameters = map[string]bool{
	"allow_system_table_mods": true, "archive_cleanup_command": true,
	"archive_command": true, "archive_mode": true, "cluster_name": true,
	"config_file": true, "data_directory": true, "external_pid_file": true,
	"full_page_writes": true, "hba_file": true, "hot_standby": true, "ident_file": true,
	"listen_addresses": true, "log_destination": true, "log_directory": true,
	"log_filename": true, "logging_collector": true, "port": true,
	"primary_conninfo": true, "primary_slot_name": true, "recovery_target": true,
	"restart_after_crash": true, "restore_command": true,
	"shared_preload_libraries": true, "ssl": true, "ssl_ca_file": true,
	"ssl_cert_file": true, "ssl_key_file": true, "synchronous_standby_names": true,
	"unix_socket_directories": true, "unix_socket_group": true,
	"unix_socket_permissions": true, "wal_log_hints": true,
}

func validateCNPGTarget(t Target) error {
	for _, f := range [][2]string{{"cluster", t.Cluster}, {"namespace", t.Namespace}} {
		if f[1] != "" && !rfc1123Label.MatchString(f[1]) {
			return fmt.Errorf("%w: %s %q is not an RFC 1123 label", ErrInvalidTarget, f[0],
				f[1])
		}
	}
	return nil
}

// cnpgValue keeps the PostgreSQL form, after the range and unit checks the
// other providers' conversions apply.
func cnpgValue(parameter, value string) (string, string, error) {
	doc, documented := pgconf.Docs[parameter]
	if documented && (doc.Unit == "bytes" || doc.Unit == "ms") {
		if _, _, err := baseUnitValue(parameter, value, doc); err != nil {
			return "", "", err
		}
		return value, "", nil
	}
	if !plainNumber.MatchString(value) && !keyword.MatchString(value) {
		return "", "", fmt.Errorf("%w: %q for %s has a unit pg_sage cannot convert",
			ErrInvalidValue, value, parameter)
	}
	if documented && plainNumber.MatchString(value) {
		if ok, why := pgconf.ValidateValue(parameter, value); !ok {
			return "", "", fmt.Errorf("%w: %s", ErrInvalidValue, why)
		}
	}
	return value, "", nil
}

// cnpgRunningForm is a Cluster value as pg_settings.setting shows it: a
// memory or time value in the setting's base unit, anything else as is.
func cnpgRunningForm(parameter, value string) string {
	doc, documented := pgconf.Docs[parameter]
	if !documented || (doc.Unit != "bytes" && doc.Unit != "ms") {
		return value
	}
	if v, _, err := baseUnitValue(parameter, value, doc); err == nil {
		return v
	}
	return value
}

// comparableValue is a configured value in the form DetectDrift compares
// with pg_settings.
func comparableValue(t Target, name, configured string) string {
	if NormalizeProvider(t.Provider) == "cnpg" {
		return cnpgRunningForm(name, configured)
	}
	return configured
}

func cnpgConfiguredValues(t Target) map[string]string {
	out := map[string]string{}
	if t.Known && t.FlagsKnown {
		for k, v := range t.Flags {
			out[k] = v
		}
	}
	return out
}

// buildCNPG fills a Cluster spec change.
func buildCNPG(p *Proposal, t Target) error {
	if cnpgFixedParameters[p.Parameter] {
		return fmt.Errorf("%w: CloudNativePG fixes %s (the operator owns it)",
			ErrNotModifiable, p.Parameter)
	}
	p.Mechanism = MechanismClusterSpec
	p.Cluster, p.Namespace = t.Cluster, t.Namespace
	cluster := orPlaceholder(t.Cluster, placeholderCluster)
	namespace := orPlaceholder(t.Namespace, placeholderNamespace)
	p.Target = namespace + "/" + cluster
	p.RebootRequired = rebootFor(p.Parameter, GroupParam{}, false)
	p.ApplyMethod = ApplyImmediate
	if p.RebootRequired {
		p.ApplyMethod = ApplyRollingRestart
	}
	value := p.Value
	p.Patch = cnpgPatch(p.Parameter, &value)
	p.CLI = kubectlPatch(cluster, namespace, p.Patch)
	p.GitOps = cnpgGitOps(cluster, namespace, p.Parameter, p.Value)
	p.Rollback = Rollback{ApplyMethod: p.ApplyMethod}
	known := t.Known && t.FlagsKnown
	prior, hadPrior := t.Flags[p.Parameter]
	if known && hadPrior {
		p.CurrentValue = prior
	}
	cnpgRollback(p, cluster, namespace, known, hadPrior, prior)
	cnpgNotes(p, t)
	return nil
}

// cnpgRollback restores the prior value, or removes the key (null) when the
// Cluster did not set it. An unknown or unquotable prior gives no command.
func cnpgRollback(p *Proposal, cluster, namespace string, known, hadPrior bool,
	prior string) {
	unsafe := checkProviderValue(p.Parameter, prior)
	switch {
	case !known:
		p.Rollback.CLI = fmt.Sprintf("# fill in: read the current value first (kubectl get "+
			"cluster %s -n %s -o jsonpath='{.spec.postgresql.parameters.%s}'), then patch "+
			"it back, or patch it to null if the Cluster did not set it", cluster,
			namespace, p.Parameter)
	case hadPrior && unsafe != nil:
		p.Rollback.CLI = "# no rollback command: see the blockers"
		p.Blockers = append(p.Blockers, unsafe.Error()+": note the Cluster's current "+
			"value before this change and restore it in the manifest if it has to be undone")
	case hadPrior:
		p.Rollback.Value = prior
		p.Rollback.Patch = cnpgPatch(p.Parameter, &prior)
		p.Rollback.CLI = kubectlPatch(cluster, namespace, p.Rollback.Patch)
	default:
		p.Rollback.Reset = true
		p.Rollback.Patch = cnpgPatch(p.Parameter, nil)
		p.Rollback.CLI = kubectlPatch(cluster, namespace, p.Rollback.Patch)
	}
}

// cnpgPatch is the merge patch of one parameter; nil removes the key.
func cnpgPatch(parameter string, value *string) string {
	patch := map[string]any{"spec": map[string]any{"postgresql": map[string]any{
		"parameters": map[string]*string{parameter: value}}}}
	// Marshalling maps of strings cannot fail.
	raw, _ := json.Marshal(patch)
	return string(raw)
}

func kubectlPatch(cluster, namespace, patch string) string {
	return fmt.Sprintf("kubectl patch cluster %s -n %s --type merge -p %s", cluster,
		namespace, shellQuote(patch))
}

// cnpgGitOps is the change as a Cluster manifest fragment for a pull
// request. The value is validated (no quotes or backslashes), so a
// double-quoted YAML scalar holds it as is.
func cnpgGitOps(cluster, namespace, parameter, value string) string {
	return fmt.Sprintf("apiVersion: postgresql.cnpg.io/v1\nkind: Cluster\nmetadata:\n"+
		"  name: %s\n  namespace: %s\nspec:\n  postgresql:\n    parameters:\n"+
		"      %s: %q\n", cluster, namespace, parameter, value)
}

func cnpgNotes(p *Proposal, t Target) {
	if !t.Known {
		p.Notes = append(p.Notes, "The CloudNativePG Cluster is unknown ("+
			orPlaceholder(t.Unresolved, "databases[].cnpg is not configured")+"): replace "+
			"<cluster> and <namespace> with the Cluster that runs this database.")
	}
	p.Notes = append(p.Notes, "CloudNativePG writes postgresql.conf from "+
		"spec.postgresql.parameters and disables ALTER SYSTEM. If a GitOps tool (Argo CD, "+
		"Flux) owns the Cluster, commit the fragment instead of patching, or its next sync "+
		"reverts the patch.")
	if !p.RebootRequired {
		p.Notes = append(p.Notes, fmt.Sprintf("CloudNativePG applies %s with a "+
			"configuration reload on every instance; no restart.", p.Parameter))
		return
	}
	strategy := orPlaceholder(t.UpdateStrategy, "unsupervised")
	p.Notes = append(p.Notes, fmt.Sprintf("%s needs a restart: CloudNativePG does a "+
		"rolling restart, the replicas one at a time and then the primary. Under "+
		"primaryUpdateStrategy: unsupervised (the default) it goes ahead at once and, "+
		"depending on primaryUpdateMethod, restarts the primary in place or does a "+
		"switchover to a replica: a short write outage either way. Under supervised it "+
		"waits for you to promote or restart the primary (kubectl cnpg promote or "+
		"restart). This Cluster uses primaryUpdateStrategy: %s.", p.Parameter, strategy))
}
