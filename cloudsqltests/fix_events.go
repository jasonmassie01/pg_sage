// +build ignore

package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("SAGE_DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close(ctx)

	_, err = conn.Exec(ctx, `
INSERT INTO events (event_type, payload, created_at)
SELECT (ARRAY['pageview','click','purchase','signup','logout'])[floor(random()*5+1)::int],
       jsonb_build_object('user_id',(random()*50000+1)::int,'page','/page/'||s),
       '2025-01-01'::timestamptz + (random()*450)::int * interval '1 day'
FROM generate_series(1,200000) s`)
	if err != nil {
		log.Fatalf("insert events: %v", err)
	}

	var count int64
	conn.QueryRow(ctx, "SELECT count(*) FROM events").Scan(&count)
	fmt.Printf("events: %d rows\n", count)

	// Run ANALYZE on events
	_, err = conn.Exec(ctx, "ANALYZE events")
	if err != nil {
		fmt.Printf("analyze: %v\n", err)
	}
}
