package main

import (
	"sync"

	"github.com/pg-sage/sidecar/internal/changereview"
)

var (
	changeReviewRegistryOnce sync.Once
	changeReviewRegistry     *changereview.Registry
)

// changeReviewServices is the process-wide Change Review registry: each
// database runtime registers its service there while it runs, and the
// API, MCP and the receipts job route through it.
func changeReviewServices() *changereview.Registry {
	changeReviewRegistryOnce.Do(func() { changeReviewRegistry = changereview.NewRegistry() })
	return changeReviewRegistry
}
