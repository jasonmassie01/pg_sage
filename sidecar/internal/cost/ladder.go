package cost

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/cloudtel"
)

// The size ladders right-sizing steps along: one size at a time, inside
// the same family. A size the region or engine does not offer is the
// operator's check before acting on the advice.

var (
	rdsSizes = []string{"micro", "small", "medium", "large", "xlarge", "2xlarge",
		"4xlarge", "8xlarge", "12xlarge", "16xlarge", "24xlarge", "32xlarge", "48xlarge"}
	n1VCPUs     = []int{1, 2, 4, 8, 16, 32, 64, 96}
	customTier  = regexp.MustCompile(`^db-custom-([0-9]+)-([0-9]+)$`)
	n1Tier      = regexp.MustCompile(`^db-n1-(standard|highmem)-([0-9]+)$`)
	maxCustomCP = 96
)

// nextSmaller is one size down in class's family ("" when none is known).
func nextSmaller(class string) string { return step(class, -1) }

// nextLarger is one size up in class's family ("" when none is known).
func nextLarger(class string) string { return step(class, +1) }

func step(class string, dir int) string {
	if strings.HasPrefix(class, "db.") {
		return rdsStep(class, dir)
	}
	if m := customTier.FindStringSubmatch(class); m != nil {
		return customStep(m[1], m[2], dir)
	}
	if m := n1Tier.FindStringSubmatch(class); m != nil {
		n, _ := strconv.Atoi(m[2])
		i := slices.Index(n1VCPUs, n)
		if i < 0 || i+dir < 0 || i+dir >= len(n1VCPUs) {
			return ""
		}
		return fmt.Sprintf("db-n1-%s-%d", m[1], n1VCPUs[i+dir])
	}
	return ""
}

// rdsStep is the neighbouring size of an RDS class when that class is
// documented (a fixed-share family starts at large).
func rdsStep(class string, dir int) string {
	parts := strings.Split(strings.ToLower(class), ".")
	if len(parts) != 3 {
		return ""
	}
	i := slices.Index(rdsSizes, parts[2])
	if i < 0 || i+dir < 0 || i+dir >= len(rdsSizes) {
		return ""
	}
	target := "db." + parts[1] + "." + rdsSizes[i+dir]
	if cloudtel.InstanceClassVCPUs(target) == 0 {
		return ""
	}
	return target
}

// customStep halves or doubles a custom tier's vCPUs and memory. A single
// vCPU is never a downsize target: it leaves no headroom for maintenance.
func customStep(cpus, mb string, dir int) string {
	n, _ := strconv.Atoi(cpus)
	m, _ := strconv.Atoi(mb)
	if dir < 0 {
		if n/2 < 2 || n%2 != 0 || m%2 != 0 || (m/2)%256 != 0 {
			return ""
		}
		return fmt.Sprintf("db-custom-%d-%d", n/2, m/2)
	}
	if 2*n > maxCustomCP {
		return ""
	}
	return fmt.Sprintf("db-custom-%d-%d", 2*n, 2*m)
}

// classVCPUs is the documented vCPU count of an RDS class or Cloud SQL tier.
func classVCPUs(class string) int {
	if strings.HasPrefix(class, "db.") {
		return cloudtel.InstanceClassVCPUs(class)
	}
	return cloudtel.TierVCPUs(class)
}

// classMemoryBytes is the documented RAM of a class or tier (0: unknown).
func classMemoryBytes(class string) float64 {
	if strings.HasPrefix(class, "db.") {
		return cloudtel.InstanceClassMemoryBytes(class)
	}
	return cloudtel.TierMemoryBytes(class)
}
