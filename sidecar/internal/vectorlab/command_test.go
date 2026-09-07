package vectorlab

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

func writeManifest(t *testing.T, m Manifest) string {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "workload.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCommandInputFailuresAndHelp(t *testing.T) {
	path := writeManifest(t, validManifest())
	for _, tc := range []struct {
		args []string
		dsn  string
		code int
	}{
		{[]string{"--help"}, "", 0}, {nil, "", 1},
		{[]string{"--unknown"}, "", 1},
		{[]string{"--manifest", "does-not-exist.json"}, "", 1},
		{[]string{"--manifest", path}, "", 1},
		{[]string{"--manifest", path}, "postgres://secret:credential@%broken", 1},
	} {
		var out, diagnostic bytes.Buffer
		code := Command(t.Context(), tc.args, tc.dsn, &out, &diagnostic)
		if code != tc.code || out.Len() != 0 || strings.Contains(diagnostic.String(), "credential") {
			t.Fatalf("command = %d out=%q err=%q", code, out.String(), diagnostic.String())
		}
	}
}

func TestCommandRealReportAndInconclusiveExit(t *testing.T) {
	p := testPool(t)
	m := seedVectors(t, p)
	for _, empty := range []bool{false, true} {
		if empty {
			for i := range m.Queries {
				m.Queries[i].Filters = []string{"999"}
			}
		}
		path := writeManifest(t, m)
		var out, diagnostic bytes.Buffer
		code := Command(t.Context(), []string{"--manifest", path},
			testdb.SkipUnlessLive(t), &out, &diagnostic)
		want := 0
		if empty {
			want = 2
		}
		var r Report
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		if code != want || r.FormatVersion != 1 || diagnostic.Len() != 0 {
			t.Fatalf("command = %d, report=%#v err=%q", code, r, diagnostic.String())
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestCommandOutputFailure(t *testing.T) {
	p := testPool(t)
	path := writeManifest(t, seedVectors(t, p))
	code := Command(t.Context(), []string{"--manifest", path}, testdb.SkipUnlessLive(t),
		failingWriter{}, io.Discard)
	if code != 1 {
		t.Fatalf("output failure exit = %d", code)
	}
}
