package firstlook

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseInt2Vector(t *testing.T) {
	cases := []struct {
		in   string
		want []int16
	}{
		{"1", []int16{1}},
		{"2 1 7", []int16{2, 1, 7}},
		{"0 0", []int16{0, 0}}, // expression columns
		{"32767 -32768", []int16{32767, -32768}},
		{"", []int16{}},
		{"  3  4 ", []int16{3, 4}},
	}
	for _, c := range cases {
		got, err := parseInt2Vector(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got == nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestParseInt2VectorRejectsMalformed(t *testing.T) {
	for _, in := range []string{"a", "1 x", "32768", "-32769", "1.5", "{1,2}", "1,2"} {
		got, err := parseInt2Vector(in)
		if err == nil {
			t.Errorf("%q: parsed as %v, want an error", in, got)
			continue
		}
		if !strings.Contains(err.Error(), "int2vector") {
			t.Errorf("%q: error %q does not name the type", in, err)
		}
	}
}

func TestParseOIDVector(t *testing.T) {
	cases := []struct {
		in   string
		want []uint32
	}{
		{"1978", []uint32{1978}},
		{"3124 3126", []uint32{3124, 3126}},
		{"0 100", []uint32{0, 100}}, // no collation, then "C"
		{"4294967295", []uint32{4294967295}},
		{"", []uint32{}},
	}
	for _, c := range cases {
		got, err := parseOIDVector(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got == nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestParseOIDVectorRejectsMalformed(t *testing.T) {
	for _, in := range []string{"-1", "4294967296", "x", "1 2 y", "0x10"} {
		if got, err := parseOIDVector(in); err == nil {
			t.Errorf("%q: parsed as %v, want an error", in, got)
		} else if !strings.Contains(err.Error(), "oidvector") {
			t.Errorf("%q: error %q does not name the type", in, err)
		}
	}
}

// Each flag bit sets exactly its own field.
func TestIndexFlags(t *testing.T) {
	fields := func(x Index) [5]bool {
		return [5]bool{x.Unique, x.Primary, x.ConstraintBacked, x.Valid, x.Ready}
	}
	for bit := range 5 {
		var x Index
		x.setFlags(1 << bit)
		var want [5]bool
		want[bit] = true
		if got := fields(x); got != want {
			t.Errorf("bit %d: got %v, want %v", bit, got, want)
		}
	}
	var none Index
	none.setFlags(0)
	if got := fields(none); got != [5]bool{} {
		t.Errorf("no bits: got %v", got)
	}
	var all Index
	all.setFlags(0x1f)
	if got := fields(all); got != [5]bool{true, true, true, true, true} {
		t.Errorf("all bits: got %v", got)
	}
}
