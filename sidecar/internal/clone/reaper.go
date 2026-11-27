package clone

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Reaper destroys the clones a crash or a dead process left behind (spec
// §6.12, lab-05a): receipts of its purpose past their TTL, and receipts of
// any purpose whose holder is another process that stopped heartbeating.
// A receipt that names no resource (creating, create_uncertain) is
// resolved by name when the provider is a Finder; otherwise it is
// reported, never destroyed: nothing says which resource it is, and a
// guess could destroy someone else's.
type Reaper struct {
	Store *ReceiptStore
	// Provider is the adapter itself, not a ReceiptingProvider: the reaper
	// moves the receipts.
	Provider Provider
	Scope    ReceiptScope
	Purpose  string
	// Holder is this process's holder id (ProcessHolder); "" reaps by TTL
	// only. A process never liveness-reaps the clones it holds itself.
	Holder string
	// StaleAfter is how old a heartbeat is before its holder is dead;
	// 0 is DefaultHolderStaleAfter.
	StaleAfter time.Duration
	// AsOf judges heartbeats at a fixed time (tests); zero is the
	// database clock.
	AsOf time.Time
}

// ReapReport is one pass: the receipts whose clones were destroyed, the
// receipts an operator must resolve, the teardowns that failed (retried on
// the next pass) and the lookups by name that failed.
type ReapReport struct {
	Destroyed  []Receipt
	Unresolved []Receipt
	Failed     map[int64]error
	FindFailed map[int64]error
}

func (r Reaper) query() ReapQuery {
	stale := r.StaleAfter
	if stale <= 0 {
		stale = DefaultHolderStaleAfter
	}
	return ReapQuery{Scope: r.Scope, Purpose: r.Purpose, Holder: r.Holder,
		StaleAfter: stale, AsOf: r.AsOf}
}

// Reap runs one pass. Its error is a failure to read the receipts; a
// single teardown failure is in the report.
func (r Reaper) Reap(ctx context.Context) (ReapReport, error) {
	if r.Provider == nil {
		return ReapReport{}, errors.New("clone reaper: provider is required")
	}
	q := r.query()
	due, err := r.Store.Reapable(ctx, q)
	if err != nil {
		return ReapReport{}, err
	}
	report := ReapReport{Failed: map[int64]error{}, FindFailed: map[int64]error{}}
	for _, receipt := range due {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		if receipt.Ref == "" {
			r.resolve(ctx, q, receipt, &report)
			continue
		}
		claimed, ok, err := r.Store.ClaimForReap(ctx, q, receipt.ID)
		if err != nil {
			report.Failed[receipt.ID] = err
			continue
		}
		if ok {
			r.destroy(ctx, claimed, &report)
		}
	}
	return report, nil
}

// resolve finds the resource of a receipt that names none. A failed
// lookup leaves it unresolved; a resource not found closes it once it is
// expired.
func (r Reaper) resolve(ctx context.Context, q ReapQuery, receipt Receipt,
	report *ReapReport) {
	finder, ok := r.Provider.(Finder)
	if !ok {
		report.Unresolved = append(report.Unresolved, receipt)
		return
	}
	name := ResourceName(receipt)
	ref, found, err := finder.FindByName(ctx, name)
	if err != nil {
		report.Unresolved = append(report.Unresolved, receipt)
		report.FindFailed[receipt.ID] = fmt.Errorf("find clone %s of receipt %d: %w", name,
			receipt.ID, err)
		return
	}
	if !found {
		closed, err := r.Store.CloseUnfound(ctx, q, receipt.ID)
		if err != nil {
			report.Failed[receipt.ID] = err
		} else if !closed {
			report.Unresolved = append(report.Unresolved, receipt)
		}
		return
	}
	claimed, ok, err := r.Store.ResolveFound(ctx, q, receipt.ID, ref)
	if err != nil {
		report.Failed[receipt.ID] = err
		return
	}
	if ok {
		r.destroy(ctx, claimed, report)
	}
}

// destroy tears down a claimed receipt's clone and closes the receipt; a
// failure leaves it destroying, retried on the next pass.
func (r Reaper) destroy(ctx context.Context, receipt Receipt, report *ReapReport) {
	destroyCtx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	if err := r.Provider.Destroy(destroyCtx, Clone{ID: receipt.Ref}); err != nil {
		report.Failed[receipt.ID] = fmt.Errorf("destroy clone %s (receipt %d): %w",
			receipt.ProviderRef(), receipt.ID, err)
		return
	}
	done, err := r.Store.MarkDestroyed(ctx, receipt.ID)
	if err != nil {
		report.Failed[receipt.ID] = err
		return
	}
	report.Destroyed = append(report.Destroyed, done)
}
