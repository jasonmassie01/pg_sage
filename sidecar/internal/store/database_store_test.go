package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func validInput() DatabaseInput {
	return DatabaseInput{
		Name:           "prod-db",
		Host:           "db.example.com",
		Port:           5432,
		DatabaseName:   "myapp",
		Username:       "admin",
		Password:       "s3cret",
		SSLMode:        "require",
		MaxConnections: 5,
		Tags:           map[string]string{"env": "prod"},
		TrustLevel:     "observation",
		ExecutionMode:  "approval",
	}
}

func TestDatabaseInputValidation(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*DatabaseInput)
		create  bool
		wantErr bool
	}{
		{
			name:    "valid create",
			modify:  func(_ *DatabaseInput) {},
			create:  true,
			wantErr: false,
		},
		{
			name:    "empty name",
			modify:  func(d *DatabaseInput) { d.Name = "" },
			create:  true,
			wantErr: true,
		},
		{
			name: "name too long",
			modify: func(d *DatabaseInput) {
				d.Name = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" +
					"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
			create:  true,
			wantErr: true,
		},
		{
			name:    "empty host",
			modify:  func(d *DatabaseInput) { d.Host = "" },
			create:  true,
			wantErr: true,
		},
		{
			name:    "port zero",
			modify:  func(d *DatabaseInput) { d.Port = 0 },
			create:  true,
			wantErr: true,
		},
		{
			name:    "port too high",
			modify:  func(d *DatabaseInput) { d.Port = 70000 },
			create:  true,
			wantErr: true,
		},
		{
			name:    "negative port",
			modify:  func(d *DatabaseInput) { d.Port = -1 },
			create:  true,
			wantErr: true,
		},
		{
			name:    "empty database_name",
			modify:  func(d *DatabaseInput) { d.DatabaseName = "" },
			create:  true,
			wantErr: true,
		},
		{
			name:    "empty username",
			modify:  func(d *DatabaseInput) { d.Username = "" },
			create:  true,
			wantErr: true,
		},
		{
			name:    "empty password on create",
			modify:  func(d *DatabaseInput) { d.Password = "" },
			create:  true,
			wantErr: true,
		},
		{
			name:    "empty password on update ok",
			modify:  func(d *DatabaseInput) { d.Password = "" },
			create:  false,
			wantErr: false,
		},
		{
			name:    "invalid sslmode",
			modify:  func(d *DatabaseInput) { d.SSLMode = "bogus" },
			create:  true,
			wantErr: true,
		},
		{
			name:    "invalid trust_level",
			modify:  func(d *DatabaseInput) { d.TrustLevel = "yolo" },
			create:  true,
			wantErr: true,
		},
		{
			name: "invalid execution_mode",
			modify: func(d *DatabaseInput) {
				d.ExecutionMode = "nope"
			},
			create:  true,
			wantErr: true,
		},
		{
			name:    "valid sslmode disable",
			modify:  func(d *DatabaseInput) { d.SSLMode = "disable" },
			create:  true,
			wantErr: false,
		},
		{
			name:    "valid sslmode verify-full",
			modify:  func(d *DatabaseInput) { d.SSLMode = "verify-full" },
			create:  true,
			wantErr: false,
		},
		{
			name: "valid trust_level autonomous",
			modify: func(d *DatabaseInput) {
				d.TrustLevel = "autonomous"
			},
			create:  true,
			wantErr: false,
		},
		{
			name: "valid execution_mode auto",
			modify: func(d *DatabaseInput) {
				d.ExecutionMode = "auto"
			},
			create:  true,
			wantErr: false,
		},
		{
			name:    "port 1 valid",
			modify:  func(d *DatabaseInput) { d.Port = 1 },
			create:  true,
			wantErr: false,
		},
		{
			name:    "port 65535 valid",
			modify:  func(d *DatabaseInput) { d.Port = 65535 },
			create:  true,
			wantErr: false,
		},
		{
			name: "name exactly 63 chars",
			modify: func(d *DatabaseInput) {
				d.Name = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" +
					"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
			create:  true,
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := validInput()
			tc.modify(&input)
			err := validateInput(input, tc.create)
			if tc.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestDatabaseStoreCreateRequiresEncryptionKey(t *testing.T) {
	s := NewDatabaseStore(nil, nil)
	_, err := s.Create(context.Background(), validInput(), 1)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("Create error = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "encryption_key") {
		t.Fatalf("Create error = %v, want encryption_key message", err)
	}
}

func TestDatabaseStoreUpdatePasswordRequiresEncryptionKey(t *testing.T) {
	s := NewDatabaseStore(nil, nil)
	err := s.Update(context.Background(), 1, validInput())
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("Update error = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "encryption_key") {
		t.Fatalf("Update error = %v, want encryption_key message", err)
	}
}
