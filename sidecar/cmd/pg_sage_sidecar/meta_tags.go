package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/fleetlearn"
	"github.com/pg-sage/sidecar/internal/store"
)

// recordTags converts a registration's tag map into the fleet's tag list:
// "key=value" strings sorted bytewise, so equal maps give equal lists. A nil
// or empty map gives nil.
func recordTags(tags map[string]string) []string {
	if len(tags) == 0 {
		return nil
	}
	out := make([]string, 0, len(tags))
	for key, value := range tags {
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out
}

// metaBoundarySummary counts registrations per fleet-learning boundary for
// the startup log; untagged databases share the "shared" boundary.
func metaBoundarySummary(records []store.DatabaseRecord) string {
	if len(records) == 0 {
		return "none"
	}
	counts := map[string]int{}
	for _, rec := range records {
		boundary := fleetlearn.Boundary(recordTags(rec.Tags))
		if boundary == "" {
			boundary = "shared"
		}
		counts[boundary]++
	}
	labels := make([]string, 0, len(counts))
	for label := range counts {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	parts := make([]string, 0, len(labels))
	for _, label := range labels {
		parts = append(parts, fmt.Sprintf("%s=%d", label, counts[label]))
	}
	return strings.Join(parts, " ")
}
