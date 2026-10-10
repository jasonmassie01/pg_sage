package decommission

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func ackDB(t *testing.T, label string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool, ctx := freshDB(t, label)
	bootstrapped(t, ctx, pool)
	applyLegacySchema(t, ctx, pool)
	seedEstate(t, ctx, pool)
	return pool, ctx
}

type ackRow struct{ id, by, source string }

func ackRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []ackRow {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT resource_id, acknowledged_by, source
		FROM sage.agentdb_decommission WHERE exported ORDER BY resource_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []ackRow
	for rows.Next() {
		var r ackRow
		if err := rows.Scan(&r.id, &r.by, &r.source); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestAcknowledge_RecordsActorAndShowsInTheInventory(t *testing.T) {
	pool, ctx := ackDB(t, "decom_ack")
	res, err := Acknowledge(ctx, pool, AckRequest{Exported: true, AcknowledgedResources: []string{
		"provider_resource:d-live", "rds_final_snapshot:d-live", "provider_resource:d-live"}},
		"admin@example.test", SourceAPI)
	if err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	if !reflect.DeepEqual(res.Acknowledged,
		[]string{"provider_resource:d-live", "rds_final_snapshot:d-live"}) ||
		len(res.AlreadyAcknowledged) != 0 {
		t.Fatalf("result = %+v", res)
	}
	want := []ackRow{{"provider_resource:d-live", "admin@example.test", "api"},
		{"rds_final_snapshot:d-live", "admin@example.test", "api"}}
	if got := ackRows(t, ctx, pool); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %+v", got)
	}
	inv, err := Build(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Unacknowledged != len(inv.Items)-2 {
		t.Fatalf("unacknowledged = %d of %d", inv.Unacknowledged, len(inv.Items))
	}
	again, err := Acknowledge(ctx, pool, AckRequest{Exported: true,
		AcknowledgedResources: []string{"provider_resource:d-live", "provider_resource:d-op"}},
		"other@example.test", SourceYAML)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.AlreadyAcknowledged, []string{"provider_resource:d-live"}) ||
		!reflect.DeepEqual(again.Acknowledged, []string{"provider_resource:d-op"}) {
		t.Fatalf("re-ack = %+v", again)
	}
	if got := ackRows(t, ctx, pool); got[0].by != "admin@example.test" || len(got) != 3 {
		t.Fatalf("a re-ack rewrote the first actor: %+v", got)
	}
}

func TestAcknowledge_RefusesWithoutAnythingWritten(t *testing.T) {
	pool, ctx := ackDB(t, "decom_ack_bad")
	cases := []struct {
		name string
		req  AckRequest
		want error
	}{
		{"not exported", AckRequest{AcknowledgedResources: []string{"provider_resource:d-op"}},
			ErrNotExported},
		{"empty", AckRequest{Exported: true}, ErrNoResources},
		{"blank ids", AckRequest{Exported: true, AcknowledgedResources: []string{" ", ""}},
			ErrNoResources},
	}
	for _, c := range cases {
		if _, err := Acknowledge(ctx, pool, c.req, "a", SourceAPI); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	_, err := Acknowledge(ctx, pool, AckRequest{Exported: true, AcknowledgedResources: []string{
		"provider_resource:d-op", "provider_resource:nope", "zzz"}}, "a", SourceAPI)
	var unknown *UnknownResourcesError
	if !errors.As(err, &unknown) || !reflect.DeepEqual(unknown.IDs,
		[]string{"provider_resource:nope", "zzz"}) {
		t.Fatalf("unknown ids err = %v", err)
	}
	if _, err := Acknowledge(ctx, pool, AckRequest{Exported: true,
		AcknowledgedResources: []string{"provider_resource:d-op"}}, "", SourceAPI); err == nil {
		t.Fatal("an acknowledgement without an actor was accepted")
	}
	if _, err := Acknowledge(ctx, pool, AckRequest{Exported: true,
		AcknowledgedResources: []string{"provider_resource:d-op"}}, "a", "chat"); err == nil {
		t.Fatal("an unknown source was accepted")
	}
	if got := ackRows(t, ctx, pool); len(got) != 0 {
		t.Fatalf("a refused acknowledgement wrote rows: %+v", got)
	}
}

func TestAcknowledge_ConcurrentCallersRecordEachResourceOnce(t *testing.T) {
	pool, ctx := ackDB(t, "decom_ack_conc")
	ids := []string{"provider_resource:d-op", "provider_resource:d-receipt",
		"local_schema:d-local"}
	var mu sync.Mutex
	var acked []string
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := Acknowledge(ctx, pool, AckRequest{Exported: true,
				AcknowledgedResources: ids}, "admin", SourceAPI)
			if err != nil {
				t.Errorf("concurrent ack: %v", err)
				return
			}
			mu.Lock()
			acked = append(acked, res.Acknowledged...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Strings(acked)
	want := append([]string(nil), ids...)
	sort.Strings(want)
	if !reflect.DeepEqual(acked, want) {
		t.Fatalf("newly acknowledged across callers = %v, want each id once", acked)
	}
	if got := ackRows(t, ctx, pool); len(got) != len(ids) {
		t.Fatalf("%d rows, want %d", len(got), len(ids))
	}
}

func TestAcknowledge_MissingTableIsDistinguishable(t *testing.T) {
	pool, ctx := legacyDB(t, "decom_ack_notable")
	seedEstate(t, ctx, pool)
	_, err := Acknowledge(ctx, pool, AckRequest{Exported: true,
		AcknowledgedResources: []string{"provider_resource:d-op"}}, "a", SourceAPI)
	if !errors.Is(err, ErrAckTableMissing) {
		t.Fatalf("err = %v, want ErrAckTableMissing", err)
	}
}
