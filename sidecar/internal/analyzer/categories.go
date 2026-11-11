package analyzer

import "github.com/pg-sage/sidecar/internal/findingcat"

// Categories returns the finding categories the analyzer's rules emit, as
// the finding category registry declares them (the rules subsystem).
func Categories() []string {
	return findingcat.Names(findingcat.ByProducer(findingcat.ProducerAnalyzer))
}
