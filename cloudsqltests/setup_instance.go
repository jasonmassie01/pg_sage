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
	host := os.Getenv("PG_HOST")
	if host == "" {
		log.Fatal("PG_HOST not set")
	}
	adminPw := os.Getenv("SAGE_ADMIN_PW")
	if adminPw == "" {
		log.Fatal("SAGE_ADMIN_PW not set")
	}
	agentPw := os.Getenv("SAGE_AGENT_PW")
	if agentPw == "" {
		log.Fatal("SAGE_AGENT_PW not set")
	}

	ctx := context.Background()

	// Connect as postgres to set up user and database
	adminURL := fmt.Sprintf("postgres://postgres:%s@%s:5432/postgres?sslmode=require", adminPw, host)
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		log.Fatalf("connect as postgres: %v", err)
	}

	steps := []struct {
		name string
		sql  string
	}{
		{"create sage_agent user", fmt.Sprintf(`
DO $$ BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'sage_agent') THEN
        CREATE USER sage_agent WITH PASSWORD '%s';
    END IF;
END $$`, agentPw)},
		{"grant pg_monitor", `GRANT pg_monitor TO sage_agent`},
		{"grant pg_read_all_stats", `GRANT pg_read_all_stats TO sage_agent`},
		{"grant pg_signal_backend", `GRANT pg_signal_backend TO sage_agent`},
	}

	for _, step := range steps {
		fmt.Printf("%s... ", step.name)
		_, err := conn.Exec(ctx, step.sql)
		if err != nil {
			fmt.Printf("ERROR: %v\n", err)
			continue
		}
		fmt.Println("OK")
	}
	conn.Close(ctx)

	// Check if sage_test database exists
	conn, err = pgx.Connect(ctx, adminURL)
	if err != nil {
		log.Fatalf("reconnect: %v", err)
	}

	var dbExists bool
	err = conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = 'sage_test')").Scan(&dbExists)
	if err != nil {
		log.Fatalf("check db: %v", err)
	}

	if !dbExists {
		fmt.Print("create sage_test database... ")
		_, err = conn.Exec(ctx, "CREATE DATABASE sage_test")
		if err != nil {
			fmt.Printf("ERROR: %v\n", err)
		} else {
			fmt.Println("OK")
		}
	} else {
		fmt.Println("sage_test database already exists")
	}
	conn.Close(ctx)

	// Connect to sage_test as postgres to set up extensions and schema
	testURL := fmt.Sprintf("postgres://postgres:%s@%s:5432/sage_test?sslmode=require", adminPw, host)
	conn, err = pgx.Connect(ctx, testURL)
	if err != nil {
		log.Fatalf("connect to sage_test: %v", err)
	}
	defer conn.Close(ctx)

	setupSteps := []struct {
		name string
		sql  string
	}{
		{"create pg_stat_statements", `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`},
		{"create sage schema", `CREATE SCHEMA IF NOT EXISTS sage`},
		{"grant sage schema", `GRANT ALL ON SCHEMA sage TO sage_agent`},
		{"alter default privileges", `ALTER DEFAULT PRIVILEGES IN SCHEMA sage GRANT ALL ON TABLES TO sage_agent`},
		{"grant create on public", `GRANT CREATE ON SCHEMA public TO sage_agent`},
		{"grant create on database", `GRANT CREATE ON DATABASE sage_test TO sage_agent`},
	}

	for _, step := range setupSteps {
		fmt.Printf("%s... ", step.name)
		_, err := conn.Exec(ctx, step.sql)
		if err != nil {
			fmt.Printf("ERROR: %v\n", err)
			continue
		}
		fmt.Println("OK")
	}

	// Verify PG version
	var version string
	conn.QueryRow(ctx, "SELECT version()").Scan(&version)
	fmt.Printf("\nPostgreSQL: %s\n", version)

	fmt.Println("\nInstance setup complete.")
}
