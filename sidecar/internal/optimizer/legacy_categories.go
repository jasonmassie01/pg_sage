package optimizer

// legacyCategories are the finding categories releases before v1.8.0
// stored LLM index advice under: the optimizer kept the LLM's own label
// (its prompt offered missing_index|covering_index|partial_index|
// composite_index) until v1.8.0 fixed the category to OptimizerCategory
// and moved the label to index_category (05c3479a, G3-B05/C05). The
// Prometheus recommendation counter enumerates the same set.
var legacyCategories = []string{"covering_index", "partial_index", "composite_index"}

// optimizerMarkers are detail keys only optimizer findings carry, in every
// release: the LLM label was free text, so a category outside the known
// set is still recognised by its detail.
var optimizerMarkers = []string{
	"llm_rationale", "plan_source", "hypopg_validated", "what_if_verdict",
}

// Categories returns OptimizerCategory followed by the legacy categories.
func Categories() []string {
	return append([]string{OptimizerCategory}, legacyCategories...)
}

// IsOptimizerFinding reports whether a finding holds LLM optimizer index
// advice, by category or by the optimizer's detail markers.
func IsOptimizerFinding(category string, detail map[string]any) bool {
	for _, c := range Categories() {
		if category == c {
			return true
		}
	}
	for _, key := range optimizerMarkers {
		if _, ok := detail[key]; ok {
			return true
		}
	}
	return false
}
