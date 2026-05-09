package agentdb

import "strings"

func BuildTuningHints(ctx TuningContext) []TuningHint {
	workloads := set(ctx.WorkloadTypes)
	extensions := set(ctx.Extensions)
	hints := make([]TuningHint, 0, 4)
	if workloads["vector"] || extensions["pgvector"] {
		hints = append(hints, vectorHint())
	}
	if workloads["postgis"] || extensions["postgis"] {
		hints = append(hints, postGISHint())
	}
	if workloads["jsonb"] || workloads["json"] {
		hints = append(hints, jsonbHint())
	}
	if len(extensions) > 0 {
		hints = append(hints, extensionHint(ctx.Extensions))
	}
	return hints
}

func set(values []string) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		v := strings.ToLower(strings.TrimSpace(value))
		if v != "" {
			out[v] = true
		}
	}
	return out
}

func vectorHint() TuningHint {
	return TuningHint{
		HintID:   "pack_vector_query_shape",
		Kind:     "vector",
		Title:    "Shape pgvector queries for bounded ANN scans",
		Severity: "advisory",
		Detail: "Use a LIMIT with vector ORDER BY, pair the operator class " +
			"with the embedding distance, and keep filter selectivity visible.",
		Payload: map[string]any{
			"indexes": []string{"hnsw", "ivfflat"},
			"scope":   "query-time hints, not HNSW experiment planning",
		},
	}
}

func postGISHint() TuningHint {
	return TuningHint{
		HintID:   "pack_postgis_spatial_filters",
		Kind:     "postgis",
		Title:    "Prefer indexable spatial predicates",
		Severity: "advisory",
		Detail: "Use GiST or SP-GiST indexes, prefer ST_DWithin for radius " +
			"filters, keep SRIDs consistent, and analyze after large loads.",
	}
}

func jsonbHint() TuningHint {
	return TuningHint{
		HintID:   "pack_jsonb_index_shapes",
		Kind:     "jsonb",
		Title:    "Match JSONB indexes to access patterns",
		Severity: "advisory",
		Detail: "Use GIN jsonb_path_ops for containment-heavy workloads and " +
			"expression indexes for scalar extraction, sort, or grouping paths.",
	}
}

func extensionHint(extensions []string) TuningHint {
	return TuningHint{
		HintID:   "pack_extension_config_readiness",
		Kind:     "extension",
		Title:    "Check extension install, preload, and GUC readiness",
		Severity: "advisory",
		Detail: "Verify extension versions, shared_preload_libraries needs, " +
			"privileges, and extension-specific GUCs before agent traffic grows.",
		Payload: map[string]any{"extensions": extensions},
	}
}
