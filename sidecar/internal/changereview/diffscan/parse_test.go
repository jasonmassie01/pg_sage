package diffscan

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// The unified-diff parser: files, hunks, line numbers on both sides and
// the added-line ranges the reasoner's ±3 gate checks against. Diffs are
// untrusted input: malformed text yields what parses, never a panic.

const twoFileDiff = `diff --git a/app/models/order.rb b/app/models/order.rb
index 3b18e51..a1b2c3d 100644
--- a/app/models/order.rb
+++ b/app/models/order.rb
@@ -1,4 +1,6 @@ class Order
 class Order < ApplicationRecord
+  scope :recent, -> { order(:created_at) }
+  scope :open, -> { where(status: "open") }
   belongs_to :customer
-  has_many :items
+  has_many :items, dependent: :destroy
 end
@@ -20,3 +22,4 @@ class Order
   def total
     items.sum(:price)
+    # cached
   end
diff --git a/db/migrate/20261023_add_note.rb b/db/migrate/20261023_add_note.rb
new file mode 100644
index 0000000..e69de29
--- /dev/null
+++ b/db/migrate/20261023_add_note.rb
@@ -0,0 +1,3 @@
+class AddNote < ActiveRecord::Migration[7.1]
+  def change; add_column :orders, :note, :text; end
+end
`

func TestParse_GitDiffFilesHunksAndLineNumbers(t *testing.T) {
	files := Parse(twoFileDiff)
	if len(files) != 2 {
		t.Fatalf("files = %d, want 2: %+v", len(files), files)
	}
	f := files[0]
	if f.Path != "app/models/order.rb" || f.OldPath != "app/models/order.rb" ||
		f.Status != StatusModified || f.Binary || len(f.Hunks) != 2 {
		t.Fatalf("first file = %+v", f)
	}
	h := f.Hunks[0]
	if h.OldStart != 1 || h.OldLines != 4 || h.NewStart != 1 || h.NewLines != 6 ||
		h.Section != "class Order" || len(h.Lines) != 7 {
		t.Fatalf("first hunk = %+v", h)
	}
	want := []Line{
		{Op: ' ', Old: 1, New: 1, Text: "class Order < ApplicationRecord"},
		{Op: '+', New: 2, Text: "  scope :recent, -> { order(:created_at) }"},
		{Op: '+', New: 3, Text: `  scope :open, -> { where(status: "open") }`},
		{Op: ' ', Old: 2, New: 4, Text: "  belongs_to :customer"},
		{Op: '-', Old: 3, Text: "  has_many :items"},
		{Op: '+', New: 5, Text: "  has_many :items, dependent: :destroy"},
		{Op: ' ', Old: 4, New: 6, Text: "end"},
	}
	if !reflect.DeepEqual(h.Lines, want) {
		t.Fatalf("first hunk lines:\n got %+v\nwant %+v", h.Lines, want)
	}
	if got := f.Hunks[1].Lines[2]; got != (Line{Op: '+', New: 24, Text: "    # cached"}) {
		t.Fatalf("second hunk added line = %+v, want new line 24", got)
	}
	if got := f.AddedRanges(); !reflect.DeepEqual(got, []Range{{2, 3}, {5, 5}, {24, 24}}) {
		t.Fatalf("added ranges = %v", got)
	}
	n := files[1]
	if n.Path != "db/migrate/20261023_add_note.rb" || n.OldPath != "" ||
		n.Status != StatusAdded || len(n.AddedRanges()) != 1 ||
		n.AddedRanges()[0] != (Range{1, 3}) {
		t.Fatalf("new file = %+v, ranges %v", n, n.AddedRanges())
	}
	if !strings.Contains(n.AddedText(), "add_column :orders, :note, :text") ||
		strings.Contains(f.AddedText(), "has_many :items\n") {
		t.Fatalf("added text: %q / %q", n.AddedText(), f.AddedText())
	}
}

const statusDiff = `diff --git a/db/old.sql b/db/old.sql
deleted file mode 100644
index 1111111..0000000
--- a/db/old.sql
+++ /dev/null
@@ -1,2 +0,0 @@
-DROP TABLE x;
-DROP TABLE y;
diff --git a/models/user.py b/models/account.py
similarity index 90%
rename from models/user.py
rename to models/account.py
index 2222222..3333333 100644
--- a/models/user.py
+++ b/models/account.py
@@ -1 +1 @@
-class User(models.Model):
+class Account(models.Model):
diff --git a/web/logo.png b/web/logo.png
new file mode 100644
index 0000000..4444444
Binary files /dev/null and b/web/logo.png differ
diff --git a/docs/moved.md b/docs/renamed.md
similarity index 100%
rename from docs/moved.md
rename to docs/renamed.md
`

func TestParse_DeletedRenamedAndBinaryFiles(t *testing.T) {
	files := Parse(statusDiff)
	if len(files) != 4 {
		t.Fatalf("files = %d, want 4: %+v", len(files), files)
	}
	del := files[0]
	if del.Path != "db/old.sql" || del.Status != StatusDeleted || len(del.AddedRanges()) != 0 ||
		len(del.Hunks) != 1 || len(del.Hunks[0].Lines) != 2 || del.Hunks[0].Lines[1].Old != 2 {
		t.Fatalf("deleted file = %+v", del)
	}
	ren := files[1]
	if ren.Path != "models/account.py" || ren.OldPath != "models/user.py" ||
		ren.Status != StatusRenamed || ren.Hunks[0].OldLines != 1 || ren.Hunks[0].NewLines != 1 {
		t.Fatalf("renamed file = %+v", ren)
	}
	bin := files[2]
	if bin.Path != "web/logo.png" || !bin.Binary || bin.Status != StatusAdded ||
		len(bin.Hunks) != 0 {
		t.Fatalf("binary file = %+v", bin)
	}
	pure := files[3]
	if pure.Path != "docs/renamed.md" || pure.OldPath != "docs/moved.md" ||
		pure.Status != StatusRenamed || len(pure.Hunks) != 0 {
		t.Fatalf("pure rename = %+v", pure)
	}
}

func TestParse_PlainUnifiedDiffWithTimestampsAndNoNewlineMarker(t *testing.T) {
	diff := "--- migrations/V1.sql\t2026-10-23 10:00:00.000000000 +0000\n" +
		"+++ migrations/V1.sql\t2026-10-23 11:00:00.000000000 +0000\n" +
		"@@ -1 +1,2 @@\n" +
		" CREATE TABLE t (id int);\n" +
		"+CREATE INDEX t_id ON t (id);\n" +
		"\\ No newline at end of file\n"
	files := Parse(diff)
	if len(files) != 1 {
		t.Fatalf("files = %+v", files)
	}
	f := files[0]
	if f.Path != "migrations/V1.sql" || f.Status != StatusModified || len(f.Hunks) != 1 ||
		len(f.Hunks[0].Lines) != 2 || f.Hunks[0].OldLines != 1 {
		t.Fatalf("file = %+v", f)
	}
	if got := f.AddedRanges(); len(got) != 1 || got[0] != (Range{2, 2}) {
		t.Fatalf("ranges = %v", got)
	}
}

func TestParse_CRLFEqualsLF(t *testing.T) {
	lf := Parse(twoFileDiff)
	crlf := Parse(strings.ReplaceAll(twoFileDiff, "\n", "\r\n"))
	if !reflect.DeepEqual(lf, crlf) {
		t.Fatalf("CRLF parse differs:\n lf %+v\ncrlf %+v", lf, crlf)
	}
}

func TestParse_EmptyAndGarbageInput(t *testing.T) {
	for _, in := range []string{"", "\n\n", "not a diff at all\njust words",
		"@@ -1,2 +1,2 @@\n+orphan hunk without a file", "diff --git\n", "--- a\n"} {
		if files := Parse(in); len(files) != 0 {
			t.Errorf("Parse(%q) = %+v, want no files", in, files)
		}
	}
}

func TestParse_TruncatedHunkKeepsWhatParsed(t *testing.T) {
	diff := "diff --git a/x.sql b/x.sql\n--- a/x.sql\n+++ b/x.sql\n" +
		"@@ -1,3 +1,5 @@\n a\n+b\n"
	files := Parse(diff)
	if len(files) != 1 || len(files[0].Hunks) != 1 || len(files[0].Hunks[0].Lines) != 2 {
		t.Fatalf("truncated hunk = %+v", files)
	}
	if got := files[0].AddedRanges(); len(got) != 1 || got[0] != (Range{2, 2}) {
		t.Fatalf("ranges = %v", got)
	}
}

func TestParse_HunkStopsAtItsCountsAndBlankContextLines(t *testing.T) {
	// Some tools strip the space of an empty context line.
	diff := "diff --git a/m.py b/m.py\n--- a/m.py\n+++ b/m.py\n" +
		"@@ -1,3 +1,4 @@\n import x\n\n+import y\n z\n" +
		"this line is outside every hunk\n"
	f := Parse(diff)
	if len(f) != 1 || len(f[0].Hunks[0].Lines) != 4 {
		t.Fatalf("hunk = %+v", f)
	}
	if l := f[0].Hunks[0].Lines[1]; l != (Line{Op: ' ', Old: 2, New: 2, Text: ""}) {
		t.Fatalf("blank context line = %+v", l)
	}
	if got := f[0].AddedRanges(); got[0] != (Range{3, 3}) {
		t.Fatalf("ranges = %v", got)
	}
}

func TestNearAdded_Boundaries(t *testing.T) {
	f := File{Hunks: []Hunk{{Lines: []Line{
		{Op: '+', New: 10}, {Op: '+', New: 11}, {Op: '+', New: 12},
		{Op: ' ', New: 13}, {Op: '+', New: 20}}}}}
	cases := []struct {
		line, tol int
		want      bool
	}{
		{13, 3, true}, {15, 3, true}, {16, 3, false}, {7, 3, true}, {6, 3, false},
		{17, 3, true}, {23, 3, true}, {24, 3, false}, {10, 0, true}, {13, 0, false},
		{0, 3, false}, {-1, 3, false}, {11, -1, false},
	}
	for _, c := range cases {
		if got := f.NearAdded(c.line, c.tol); got != c.want {
			t.Errorf("NearAdded(%d, %d) = %v, want %v", c.line, c.tol, got, c.want)
		}
	}
	if (File{}).NearAdded(1, 3) {
		t.Error("a file without added lines is near nothing")
	}
}

func TestFind_NormalizesPrefixes(t *testing.T) {
	files := Parse(twoFileDiff)
	for _, p := range []string{"app/models/order.rb", "./app/models/order.rb",
		"b/app/models/order.rb", "a/app/models/order.rb"} {
		f, ok := Find(files, p)
		if !ok || f.Path != "app/models/order.rb" {
			t.Errorf("Find(%q) = %+v, %v", p, f.Path, ok)
		}
	}
	for _, p := range []string{"", "order.rb", "app/models", "app/models/order.rb.bak"} {
		if _, ok := Find(files, p); ok {
			t.Errorf("Find(%q) matched", p)
		}
	}
}

func TestExcerpt_NewSideLinesRedactedAndBounded(t *testing.T) {
	files := Parse(twoFileDiff)
	got, err := Excerpt(files[0], 2, 5, 4096)
	if err != nil {
		t.Fatal(err)
	}
	want := "2 + " + `  scope :recent, -> { order(:created_at) }` + "\n" +
		"3 + " + `  scope :open, -> { where(status: "open") }` + "\n" +
		"4   " + "  belongs_to :customer" + "\n" +
		"5 + " + "  has_many :items, dependent: :destroy" + "\n"
	if got != want {
		t.Fatalf("excerpt:\n%s\nwant:\n%s", got, want)
	}
	short, err := Excerpt(files[0], 1, 30, 60)
	if err != nil || len(short) > 60 || !strings.HasPrefix(short, "1   class Order") {
		t.Fatalf("bounded excerpt %q (%d bytes, %v)", short, len(short), err)
	}
	secret := Parse("diff --git a/db/migrate/x.rb b/db/migrate/x.rb\n--- /dev/null\n" +
		"+++ b/db/migrate/x.rb\n@@ -0,0 +1,2 @@\n+# postgres://app:s3cret@db/prod\n+x\n")
	ex, err := Excerpt(secret[0], 1, 2, 4096)
	if err != nil || strings.Contains(ex, "s3cret") || !strings.Contains(ex, Redacted) {
		t.Fatalf("excerpt not redacted: %q (%v)", ex, err)
	}
}

func TestExcerpt_RejectsBadRanges(t *testing.T) {
	f := Parse(twoFileDiff)[0]
	for _, r := range [][2]int{{0, 3}, {5, 4}, {-2, 1}, {1, 1 + MaxExcerptLines}} {
		if _, err := Excerpt(f, r[0], r[1], 4096); !errors.Is(err, ErrInvalidRange) {
			t.Errorf("Excerpt(%d, %d) = %v, want ErrInvalidRange", r[0], r[1], err)
		}
	}
	if _, err := Excerpt(f, 9, 15, 4096); !errors.Is(err, ErrNoLines) {
		t.Errorf("a range between hunks: %v, want ErrNoLines", err)
	}
	if _, err := Excerpt(f, 1, 3, 0); !errors.Is(err, ErrInvalidRange) {
		t.Errorf("zero byte cap: %v, want ErrInvalidRange", err)
	}
}

func FuzzParse(f *testing.F) {
	f.Add(twoFileDiff)
	f.Add("@@ -0,0 +1 @@\n+x")
	f.Add("diff --git a/x b/y\n--- a/x\n+++ b/y\n@@ -9999999999,1 +1,-3 @@\n+x\n")
	f.Fuzz(func(t *testing.T, in string) {
		for _, file := range Parse(in) {
			for _, r := range file.AddedRanges() {
				if r.From < 1 || r.To < r.From {
					t.Fatalf("bad range %v from %q", r, in)
				}
			}
			_ = RenderFile(file)
		}
	})
}

// No concurrent access tests: the parser, classifier and redactor are
// pure functions over their arguments and share no state.
