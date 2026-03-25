// +build ignore

package main

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx,
		"postgres://postgres:sage-alloydb-pw@34.27.168.36:5432/sage_test?sslmode=require")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close(ctx)

	cmds := []string{
		"GRANT alloydbsuperuser TO sage_agent",
		"ALTER TABLE public.customers OWNER TO sage_agent",
		"ALTER TABLE public.products OWNER TO sage_agent",
		"ALTER TABLE public.orders OWNER TO sage_agent",
		"ALTER TABLE public.line_items OWNER TO sage_agent",
		"ALTER TABLE public.order_events OWNER TO sage_agent",
		"ALTER TABLE public.audit_log OWNER TO sage_agent",
		"ALTER SEQUENCE public.ticket_seq OWNER TO sage_agent",
	}

	for _, cmd := range cmds {
		fmt.Printf("%s... ", cmd)
		_, err := conn.Exec(ctx, cmd)
		if err != nil {
			fmt.Printf("ERROR: %v\n", err)
		} else {
			fmt.Println("OK")
		}
	}

	// Also reassign indexes
	rows, _ := conn.Query(ctx, `
		SELECT schemaname, indexname FROM pg_indexes
		WHERE schemaname = 'public'`)
	defer rows.Close()
	for rows.Next() {
		var schema, idx string
		rows.Scan(&schema, &idx)
		cmd := fmt.Sprintf(
			"ALTER INDEX %s.%s OWNER TO sage_agent", schema, idx)
		_, err := conn.Exec(ctx, cmd)
		if err != nil {
			fmt.Printf("  %s: ERROR: %v\n", idx, err)
		}
	}

	fmt.Println("\nPermissions fixed.")
}
