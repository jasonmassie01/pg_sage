package wal

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestClassifyCanceledContextFailsClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	decision, err := Classify(ctx, abandonedLogicalSlot(), testPolicy())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Classify error = %v, want context canceled", err)
	}
	if decision.Action == ActionDrop {
		t.Fatalf("canceled classification produced drop: %#v", decision)
	}
}

func TestClassifyExpiredDeadlineFailsClosed(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	decision, err := Classify(ctx, abandonedLogicalSlot(), testPolicy())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Classify error = %v, want deadline exceeded", err)
	}
	if decision.Action != ActionPark {
		t.Fatalf("deadline decision = %#v, want parked", decision)
	}
}
