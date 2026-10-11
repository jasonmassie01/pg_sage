package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/vectorlab"
)

func runVectorLab() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	dsn, err := config.LookupSecretEnv("SAGE_VECTORLAB_DATABASE_URL")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "vector-lab: %v\n", err)
		return 2
	}
	return vectorlab.Command(ctx, os.Args[2:], dsn, os.Stdout, os.Stderr)
}
