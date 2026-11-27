package advisor

import (
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

// modelDetailKey is the finding Detail key under which model output that
// is neither on the detail allowlist nor reserved is kept, as data.
const modelDetailKey = "model"

// reservedDetailKeys are the Detail keys pg_sage's own producers set and
// its executor, approval cards and managed-change worker trust. Model
// output may never supply them; keys starting with sage_ are reserved too.
var reservedDetailKeys = map[string]bool{
	managedparam.DetailKey:          true,
	"what_if_verdict":               true,
	"drop_ddl":                      true,
	"index_replace":                 true,
	analyzer.DetailApprovalRequired: true,
}

// modelTopLevelKeys are the model keys that stay at the top level of a
// finding's Detail, where the dashboard and downstream readers find them.
var modelTopLevelKeys = map[string]bool{
	"rationale": true, "current_settings": true, "recommended_settings": true,
	"requires_restart": true, "peak_active": true, "bloat_pct": true,
	"remediation_options": true, "original_query": true, "suggested_rewrite": true,
	"object_identifier": true, "severity": true,
}

// categoryTopLevelKeys extends modelTopLevelKeys per category.
// isCreateIndexRewrite reads query_rewrite's recommended_sql there.
var categoryTopLevelKeys = map[string]map[string]bool{
	"query_rewrite": {"recommended_sql": true},
}

// isReservedDetailKey reports whether key is one model output may not
// set, ignoring case and surrounding space so near-misses fail closed.
func isReservedDetailKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	return reservedDetailKeys[k] || strings.HasPrefix(k, "sage_")
}

// advisorDetail builds a finding's Detail from one model recommendation:
// allowlisted keys stay top-level, other keys move under detail.model,
// and reserved keys are dropped at any depth. It returns the dropped
// keys (sorted) and never modifies rec.
func advisorDetail(category string, rec map[string]any) (map[string]any, []string) {
	detail := map[string]any{}
	model := map[string]any{}
	var dropped []string
	for key, val := range rec {
		if isReservedDetailKey(key) {
			dropped = append(dropped, key)
			continue
		}
		val = scrubReserved(val, &dropped)
		if modelTopLevelKeys[key] || categoryTopLevelKeys[category][key] {
			detail[key] = val
		} else {
			model[key] = val
		}
	}
	if len(model) > 0 {
		detail[modelDetailKey] = model
	}
	sort.Strings(dropped)
	return detail, dropped
}

// scrubReserved returns a copy of a decoded JSON value with reserved keys
// removed from every nested object, appending each removed key.
func scrubReserved(val any, dropped *[]string) any {
	switch v := val.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, inner := range v {
			if isReservedDetailKey(key) {
				*dropped = append(*dropped, key)
				continue
			}
			out[key] = scrubReserved(inner, dropped)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, inner := range v {
			out[i] = scrubReserved(inner, dropped)
		}
		return out
	}
	return val
}
