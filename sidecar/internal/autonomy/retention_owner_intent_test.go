package autonomy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// D1/D5: the intent sent to the policy gate carries the contract's
// owner-declared retention column, so the gate can tell an owner-authorized
// delete from one without a declaration.
func TestRetentionIntentCarriesDeclaredColumn(t *testing.T) {
	for _, declared := range []string{"created_at", ""} {
		var got RetentionIntent
		enforcer := &postgresRetentionEnforcer{pipeline: func(
			_ context.Context, intent RetentionIntent, _ RetentionBatch,
		) error {
			got = intent
			return errors.New("withheld by test")
		}}
		item := schemaguard.Remediation{
			Invariant: schemaguard.Invariant{Schema: "public", Table: "events",
				RetentionColumn: "created_at"},
			Contract: schemaguard.TableContract{AppendOnly: true,
				RetentionWindow: 24 * time.Hour, RetentionColumn: declared},
		}

		err := enforcer.authorizedDelete(context.Background(), retentionPlan{item: item,
			cutoff: time.Now(), candidates: 3, bound: 3})

		if err == nil {
			t.Fatal("authorizedDelete ignored the pipeline's refusal")
		}
		if got.DeclaredColumn != declared || got.Column != "created_at" {
			t.Fatalf("intent = %+v, want Column created_at DeclaredColumn %q", got, declared)
		}
	}
}
