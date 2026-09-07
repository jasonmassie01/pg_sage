package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/pg-sage/sidecar/internal/vectorlab"
)

func runVectorLab() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return vectorlab.Command(ctx, os.Args[2:], os.Getenv("SAGE_VECTORLAB_DATABASE_URL"),
		os.Stdout, os.Stderr)
}
