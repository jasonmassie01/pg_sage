package schema

import (
	"context"
	"testing"
)

// plan-03b on a real server: bootstrap three times is clean and records
// ddlPlanCardDeliveries once; the checks refuse a card that offers no
// reject, an unknown mode, an up-to button without its ordinal (and an
// ordinal without the button), a bad plan hash, a used card with no
// decision and a decision the card did not offer.
func TestPlanCardMigration_BootstrapTwiceIsClean(t *testing.T) {
	pool := freshSchemaPool(t, "plan_card_deliveries") // bootstraps twice
	ctx := context.Background()
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("third bootstrap: %v", err)
	}
	var ledger int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.schema_migrations
		WHERE id = 'ddlPlanCardDeliveries'`).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("ledger rows for ddlPlanCardDeliveries: %d (%v), want 1", ledger, err)
	}
	const insert = `INSERT INTO sage.plan_card_deliveries (token_sha256, channel_id,
		database_name, plan_id, plan_hash, offered_modes, up_to_ordinal, expires_at,
		used_at, decision)
		VALUES (sha256($1::bytea), 1, 'orders', 7, $2, $3, $4, now() + interval '1 hour',
		        $5, $6)`
	hash := "4f1c2b0d9e8a7f6e5d4c3b2a19087f6e5d4c3b2a19087f6e5d4c3b2a19087f6e"
	var none *int
	two := 2
	if _, err := pool.Exec(ctx, insert, []byte("ok-card"), hash,
		[]string{"approve_up_to", "approve_next", "reject"}, &two, nil,
		nil); err != nil {
		t.Fatalf("a valid card was refused: %v", err)
	}
	refused := map[string][]any{
		"no reject": {[]byte("a"), hash, []string{"approve_all"}, none, nil, nil},
		"unknown mode": {[]byte("b"), hash, []string{"approve_most", "reject"}, none, nil,
			nil},
		"up_to no ordinal": {[]byte("c"), hash, []string{"approve_up_to", "reject"}, none,
			nil, nil},
		"ordinal no button": {[]byte("d"), hash, []string{"approve_all", "reject"}, &two, nil, nil},
		"bad hash":          {[]byte("e"), "XYZ", []string{"reject"}, none, nil, nil},
		"used no decision":  {[]byte("f"), hash, []string{"reject"}, none, "now", nil},
		"decision not offered": {[]byte("g"), hash, []string{"approve_next", "reject"}, none,
			"now", "approve_all"},
	}
	for name, args := range refused {
		var usedAt any
		if args[4] != nil {
			usedAt = "2026-11-16T10:00:00Z"
		}
		if _, err := pool.Exec(ctx, insert, args[0], args[1], args[2], args[3], usedAt,
			args[5]); err == nil {
			t.Errorf("%s: the card was stored", name)
		}
	}
}
