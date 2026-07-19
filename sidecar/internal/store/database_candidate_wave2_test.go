package store

import (
	"context"
	"net/url"
	"testing"
)

func TestWave2UpdateConnectionCandidateUsesUnpersistedPassword(t *testing.T) {
	input := validInput()
	input.Host = "candidate.example.com"
	input.DatabaseName = "orders_v2"
	input.Username = "candidate_user"
	input.Password = "candidate-password"

	store := NewDatabaseStore(nil, nil)
	connString, err := store.GetUpdateConnectionString(
		context.Background(), 42, input,
	)
	if err != nil {
		t.Fatalf("candidate connection string: %v", err)
	}
	parsed, err := url.Parse(connString)
	if err != nil {
		t.Fatalf("parse candidate connection string: %v", err)
	}
	password, ok := parsed.User.Password()
	if !ok || password != input.Password {
		t.Fatalf("candidate password = %q, %t; want %q, true",
			password, ok, input.Password)
	}
	if parsed.Host != "candidate.example.com:5432" ||
		parsed.Path != "/orders_v2" || parsed.User.Username() != "candidate_user" {
		t.Fatalf("candidate connection identity = %q %q %q",
			parsed.Host, parsed.Path, parsed.User.Username())
	}
}

func TestWave2UpdateConnectionCandidateValidatesBeforeCredentialRead(t *testing.T) {
	input := validInput()
	input.Host = ""
	store := NewDatabaseStore(nil, nil)
	if _, err := store.GetUpdateConnectionString(
		context.Background(), 42, input,
	); err == nil {
		t.Fatal("invalid candidate unexpectedly reached credential lookup")
	}
}
