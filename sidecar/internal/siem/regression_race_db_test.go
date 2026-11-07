package siem

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// waitFor polls cond for up to ten seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// insertConcurrently inserts writers*each action_log rows from concurrent
// writers and returns the first failure.
func insertConcurrently(pool *pgxpool.Pool, writers, each int) error {
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				_, err := pool.Exec(context.Background(), `INSERT INTO sage.action_log
					(action_type, sql_executed) VALUES ('vacuum', 'VACUUM t')`)
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return fmt.Errorf("concurrent insert: %w", err)
		}
	}
	return nil
}

func mustInsertConcurrently(t *testing.T, pool *pgxpool.Pool, writers, each int) {
	t.Helper()
	if err := insertConcurrently(pool, writers, each); err != nil {
		t.Fatal(err)
	}
}

func head(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var seq int64
	err := pool.QueryRow(context.Background(), `SELECT coalesce(max(seq), 0)
		FROM sage.audit_chain_link WHERE chain = 'action_log'`).Scan(&seq)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	return seq
}

// Inserts racing the running exporter across the regression check: the
// regressed chain sends nothing while writers append, and after the
// re-anchor every position from the anchor on arrives once, in order,
// labelled a fork only where it was reused.
func TestInsertsRacingTheRegressionCheck(t *testing.T) {
	pool := freshDB(t, "siem_regress_race")
	sink := &memSink{name: "soc"}
	e := exporter(pool, unfenced, SinkConfig{Sink: sink})
	regress(t, pool, e)
	before := len(sink.exported())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	mustInsertConcurrently(t, pool, 4, 5)
	waitFor(t, "the regression", func() bool { return e.Status()[0].Status == StatusRegressed })
	if n := len(ofChain(since(sink.exported(), before), "action_log")); n != 0 {
		t.Fatalf("a regressed chain delivered %d events while writers raced", n)
	}
	if _, err := e.Reanchor(context.Background(), ReanchorRequest{Sink: "soc",
		Source: "orders", Chain: "action_log", FromSeq: 8}); err != nil {
		t.Fatalf("reanchor: %v", err)
	}
	mustInsertConcurrently(t, pool, 4, 5)
	want := head(t, pool)
	waitFor(t, "every position", func() bool {
		got := ofChain(since(sink.exported(), before), "action_log")
		return len(got) > 0 && got[len(got)-1].seq >= want
	})
	cancel()
	<-done
	got := ofChain(since(sink.exported(), before), "action_log")
	if int64(len(got)) != want-7 {
		t.Fatalf("delivered %d events after the anchor, want %d (8-%d)", len(got), want-7,
			want)
	}
	for i, ev := range got {
		if ev.seq != int64(8+i) || isFork(ev.labels) != (ev.seq <= 10) {
			t.Fatalf("event %d = %+v: want seq %d, fork label only up to 10", i, ev, 8+i)
		}
	}
}

// A re-anchor racing a running drain on a healthy chain is never undone
// by that drain: export restarts at the anchor and reaches the head with
// no position skipped.
func TestReanchorRacingADrainNeverSkips(t *testing.T) {
	pool := freshDB(t, "siem_reanchor_race")
	sink := &memSink{name: "soc"}
	e := exporter(pool, unfenced, SinkConfig{Sink: sink})
	addActions(t, pool, 3)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	waitFor(t, "the first export", func() bool { return len(sink.exported()) >= 3 })
	writers := make(chan error, 1)
	go func() { writers <- insertConcurrently(pool, 4, 25) }()
	waitFor(t, "a drain under way", func() bool { return len(sink.exported()) > 10 })
	mark := len(sink.exported())
	res, err := e.Reanchor(context.Background(), ReanchorRequest{Sink: "soc",
		Source: "orders", Chain: "action_log", FromSeq: 1})
	if err != nil {
		t.Fatalf("reanchor: %v", err)
	}
	if err := <-writers; err != nil {
		t.Fatal(err)
	}
	want := head(t, pool)
	waitFor(t, "the head after the re-anchor", func() bool {
		got := fromRestart(since(sink.exported(), mark))
		return len(got) > 0 && got[len(got)-1].seq >= want
	})
	cancel()
	<-done
	got := fromRestart(since(sink.exported(), mark))
	if int64(len(got)) != want {
		t.Fatalf("after the re-anchor delivered %d events, want 1-%d", len(got), want)
	}
	for i, ev := range got {
		if ev.seq != int64(i+1) || isFork(ev.labels) != (ev.seq <= res.PreviousSeq) {
			t.Fatalf("event %d = %+v: want seq %d, fork up to %d", i, ev, i+1,
				res.PreviousSeq)
		}
	}
}

// fromRestart is the action_log events from the re-export of position 1
// on (the first fork-labelled one): a pass under way when the re-anchor
// was called may still have sent a few events after it was asked for.
func fromRestart(all []exported) []exported {
	links := ofChain(all, "action_log")
	for i, ev := range links {
		if ev.seq == 1 && isFork(ev.labels) {
			return links[i:]
		}
	}
	return nil
}
