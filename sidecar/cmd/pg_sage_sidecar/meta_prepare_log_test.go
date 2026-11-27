package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/crypto"
)

// No concurrent-access tests: the function only formats one log line.

func TestLogStorePrepareFailure_WrongKeyIsWarnNotError(t *testing.T) {
	err := fmt.Errorf("get connection string: database 3: %w",
		crypto.ErrWrongEncryptionKey)
	out := captureStderr(t, func() { logStorePrepareFailure("orders", err) })
	if strings.Contains(out, "[ERROR]") {
		t.Errorf("wrong key logged a second ERROR:\n%s", out)
	}
	for _, want := range []string{"[WARN]", "[meta-db]", `db "orders"`, "database 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

func TestLogStorePrepareFailure_OtherErrorsStayError(t *testing.T) {
	for name, err := range map[string]error{
		"connection": errors.New("dial tcp 10.0.0.1:5432: connection refused"),
		"truncated":  errors.New("decrypting password for database 4: ciphertext too short"),
	} {
		t.Run(name, func(t *testing.T) {
			out := captureStderr(t, func() { logStorePrepareFailure("orders", err) })
			if !strings.Contains(out, "[ERROR]") || !strings.Contains(out, err.Error()) {
				t.Errorf("log = %q, want an ERROR with %q", out, err)
			}
		})
	}
}
