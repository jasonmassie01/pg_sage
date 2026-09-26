package agentdb

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// G8-B28: the per-file cap alone lets a zip expand to an unbounded total.
func TestTerraformZipCapsTotalDecompressedSize(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	chunk := strings.Repeat("a", maxTerraformTemplateBytes-1)
	for i := 0; i < maxTerraformZipFiles; i++ {
		f, err := w.Create(fmt.Sprintf("f%d.tf", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := TerraformFilesFromZip("bomb", buf.Bytes()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized total expansion err = %v, want ErrInvalid", err)
	}
}
