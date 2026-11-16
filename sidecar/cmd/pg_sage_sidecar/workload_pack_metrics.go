package main

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// writeWorkloadPackMetrics emits the workload evidence pack builds by
// result and the bytes of each database's last built or reused pack
// (evid-02a). A growing failed or too_large series means packs cannot be
// built for that database.
func writeWorkloadPackMetrics(b *strings.Builder, builds []workpack.BuildCount,
	sizes []workpack.PackSize) {
	metricsreg.Header(b, "pg_sage_workload_pack_builds_total")
	for _, c := range builds {
		fmt.Fprintf(b, "pg_sage_workload_pack_builds_total{database=%q,result=%q} %d\n",
			c.Database, c.Result, c.Count)
	}
	b.WriteString("\n")
	metricsreg.Header(b, "pg_sage_workload_pack_bytes")
	for _, s := range sizes {
		fmt.Fprintf(b, "pg_sage_workload_pack_bytes{database=%q} %d\n", s.Database, s.Bytes)
	}
	b.WriteString("\n")
}
