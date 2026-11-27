package pgconf

import (
	"reflect"
	"testing"
)

// advisors-06: version notes carry the majors they were verified for.

func TestNotesFor_Boundaries(t *testing.T) {
	d := GUCDoc{VersionNotes: []VersionNote{
		{MinMajor: 15, MaxMajor: 16, Text: "fifteen-sixteen"},
		{MinMajor: 17, Text: "seventeen-on"},
	}}
	cases := map[int][]string{
		0:  nil,
		-1: nil,
		14: nil,
		15: {"fifteen-sixteen"},
		16: {"fifteen-sixteen"},
		17: {"seventeen-on"},
		99: {"seventeen-on"},
	}
	for major, want := range cases {
		if got := d.NotesFor(major); !reflect.DeepEqual(got, want) {
			t.Errorf("NotesFor(%d) = %q, want %q", major, got, want)
		}
	}
	if got := (GUCDoc{}).NotesFor(18); got != nil {
		t.Errorf("a doc without notes gave %q", got)
	}
}

// Every shipped note is well formed: a supported lower bound, an upper
// bound that is open or not below it, and text.
func TestDocs_VersionNotesWellFormed(t *testing.T) {
	seen := 0
	for name, d := range Docs {
		for _, n := range d.VersionNotes {
			seen++
			if n.MinMajor < 14 || (n.MaxMajor != 0 && n.MaxMajor < n.MinMajor) ||
				n.Text == "" {
				t.Errorf("%s: malformed version note %+v", name, n)
			}
		}
	}
	if seen < 3 {
		t.Errorf("version notes shipped = %d, want the three verified ones", seen)
	}
}
