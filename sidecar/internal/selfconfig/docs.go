package selfconfig

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
)

// Markdown renders the generated derived-settings reference
// (docs/generated/derived-settings.md).
func Markdown() string {
	var b strings.Builder
	b.WriteString("# Derived settings\n\n")
	b.WriteString("> Generated from `internal/selfconfig` by `cmd/gen_config_meta`; ")
	b.WriteString("do not edit manually.\n\n")
	b.WriteString(markdownIntro)
	b.WriteString("| Key | Lifecycle | Bounds | Spends more when | Evidence | " +
		"Shadow comparison |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	def := config.DefaultConfig()
	for _, r := range Rules() {
		bounds := r.Bounds(def)
		lifecycle := "live"
		if isRestart(r.Key) {
			lifecycle = "restart"
		}
		direction := "lower"
		if r.Widens == WidensWhenHigher {
			direction = "higher"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %g-%g %s (default %g) | %s | %s | %s |\n",
			r.Key, lifecycle, bounds.Min, bounds.Max, r.Unit, bounds.Default, direction,
			r.EvidenceDoc, outcomeDoc(r))
	}
	b.WriteString("\n## Rules\n\n")
	for _, r := range Rules() {
		fmt.Fprintf(&b, "- `%s` (rule `%s`, version %d): %s", r.Key, r.Name, r.Version,
			r.Summary)
		if r.Cap == CapFixed {
			b.WriteString(" Fixed ceiling: " + r.CapNote)
		} else {
			b.WriteString(" Never derived past the default in the direction that " +
				"spends more.")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func outcomeDoc(r Rule) string {
	if r.OutcomeDoc != "" {
		return r.OutcomeDoc
	}
	return "none measurable; the soak is the test"
}

const markdownIntro = "pg_sage derives these keys per database when the operator " +
	"leaves them unset. Every other key is never derived: see the self-config class " +
	"in [configuration field lifecycles](config-lifecycles.md).\n\n" +
	"- **Evidence and bounds.** A rule reads the database (catalog size, scan " +
	"times, connection limit, temp-file rate, pg_sage's own cost) and clamps its " +
	"value to bounds that never widen authority or spend: past the product default " +
	"only where a fixed ceiling is justified below.\n" +
	"- **Shadow first.** A new value is recorded in shadow and soaks for " +
	"`self_config.soak_hours` (default 24). It is promoted only when the comparison " +
	"against the active value's measured outcomes is not worse; otherwise it stays " +
	"in shadow with the reason.\n" +
	"- **Restart-bound keys** change only at startup; a value promoted while running " +
	"is reported as pending restart.\n" +
	"- **The operator always wins.** A key set in the YAML file, a fleet " +
	"`databases[]`/`defaults` field or an API override is never derived and shows as " +
	"set by the operator. An admin can pin the current value (\"pin current\") or " +
	"unpin it from the Configuration page or `POST /api/v1/derived-settings/{key}/pin`; " +
	"a pinned value ignores new evidence until it is unpinned.\n" +
	"- **Ledger.** Every derivation, promotion, hold, pin and unpin is recorded in " +
	"`sage.config_derivation` with the value, previous value, cited evidence, bounds, " +
	"rule and version. `self_config.enabled: false` turns derivation off.\n\n"
