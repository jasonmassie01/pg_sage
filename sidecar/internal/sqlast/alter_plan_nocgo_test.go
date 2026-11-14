//go:build !cgo

package sqlast

import (
	"errors"
	"reflect"
	"testing"
)

func TestParsePlannableAlterFailsClosedWithoutCgo(t *testing.T) {
	for _, sql := range []string{
		"ALTER TABLE public.users ADD CONSTRAINT k UNIQUE (email)",
		"ALTER TABLE public.accounts ALTER COLUMN email SET NOT NULL",
		"",
	} {
		got, err := ParsePlannableAlter(sql)
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("ParsePlannableAlter(%q) err = %v, want ErrUnavailable", sql, err)
		}
		if !reflect.DeepEqual(got, PlannableAlter{}) {
			t.Fatalf("ParsePlannableAlter(%q) = %#v, want the zero value", sql, got)
		}
	}
}
