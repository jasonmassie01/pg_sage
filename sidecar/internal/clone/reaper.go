package clone

import (
	"context"
	"errors"
	"fmt"
)

// Reaper destroys the clones of expired receipts in one scope and purpose:
// what a crash between create and destroy left behind (spec §6.12). It
// destroys only a receipt that names its provider resource. An expired
// receipt without a ref (creating, create_uncertain) is reported, never
// destroyed: nothing says which resource it is, and a guess could destroy
// someone else's.
type Reaper struct {
	Store *ReceiptStore
	// Provider is the adapter itself, not a ReceiptingProvider: the reaper
	// moves the receipts.
	Provider Provider
	Scope    ReceiptScope
	Purpose  string
}

// ReapReport is one pass: the receipts whose clones were destroyed, the
// expired receipts an operator must resolve, and the teardowns that
// failed (retried on the next pass).
type ReapReport struct {
	Destroyed  []Receipt
	Unresolved []Receipt
	Failed     map[int64]error
}

// Reap runs one pass. Its error is a failure to read the receipts; a
// single teardown failure is in the report.
func (r Reaper) Reap(ctx context.Context) (ReapReport, error) {
	if r.Provider == nil {
		return ReapReport{}, errors.New("clone reaper: provider is required")
	}
	expired, err := r.Store.Expired(ctx, r.Scope, r.Purpose)
	if err != nil {
		return ReapReport{}, err
	}
	report := ReapReport{Failed: map[int64]error{}}
	for _, receipt := range expired {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		if receipt.Ref == "" {
			report.Unresolved = append(report.Unresolved, receipt)
			continue
		}
		done, err := r.reapOne(ctx, receipt)
		if err != nil {
			report.Failed[receipt.ID] = err
			continue
		}
		report.Destroyed = append(report.Destroyed, done)
	}
	return report, nil
}

func (r Reaper) reapOne(ctx context.Context, receipt Receipt) (Receipt, error) {
	if _, err := r.Store.MarkDestroying(ctx, receipt.ID); err != nil {
		return Receipt{}, err
	}
	destroyCtx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	if err := r.Provider.Destroy(destroyCtx, Clone{ID: receipt.Ref}); err != nil {
		return Receipt{}, fmt.Errorf("destroy expired clone %s (receipt %d): %w",
			receipt.ProviderRef(), receipt.ID, err)
	}
	return r.Store.MarkDestroyed(ctx, receipt.ID)
}
