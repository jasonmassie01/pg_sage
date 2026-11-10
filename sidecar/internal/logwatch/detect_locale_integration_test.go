//go:build integration

package logwatch

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDetectLCMessages_ReadsTheServerSetting(t *testing.T) {
	dsn := os.Getenv("SAGE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAGE_TEST_DATABASE_URL not set: needs a real PostgreSQL server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	var want string
	if err := pool.QueryRow(ctx, "SHOW lc_messages").Scan(&want); err != nil {
		t.Fatalf("SHOW lc_messages: %v", err)
	}
	got, err := DetectLCMessages(ctx, pool)
	if err != nil {
		t.Fatalf("DetectLCMessages: %v", err)
	}
	if got != want {
		t.Fatalf("DetectLCMessages = %q, want %q", got, want)
	}
}

func TestDetectLCMessages_CancelledContextIsAnError(t *testing.T) {
	dsn := os.Getenv("SAGE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAGE_TEST_DATABASE_URL not set: needs a real PostgreSQL server")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DetectLCMessages(ctx, pool); err == nil {
		t.Fatal("DetectLCMessages on a cancelled context returned no error")
	}
}
