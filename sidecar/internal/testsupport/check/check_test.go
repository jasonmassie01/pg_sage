package check

import (
	"errors"
	"testing"
	"time"
)

func TestMessageFormats(t *testing.T) {
	if got := Message(nil); got != "" {
		t.Errorf("empty msgAndArgs = %q, want empty", got)
	}
	if got := Message([]any{"plain"}); got != "plain: " {
		t.Errorf("single value = %q", got)
	}
	if got := Message([]any{"n=%d", 7}); got != "n=7: " {
		t.Errorf("format args = %q", got)
	}
	if got := Message([]any{1, 2}); got != "1 2: " {
		t.Errorf("non-string lead = %q", got)
	}
}

func TestEqualSemantics(t *testing.T) {
	if ok, _ := Equal([]byte("ab"), []byte("ab")); !ok {
		t.Error("byte slices should compare equal")
	}
	if ok, _ := Equal(5, int64(5)); ok {
		t.Error("differing numeric types must not be equal")
	}
	if ok, _ := Equal(map[string]int{"a": 1}, map[string]int{"a": 1}); !ok {
		t.Error("deep-equal maps should be equal")
	}
	if ok, _ := NotEqual(1, 2); !ok {
		t.Error("NotEqual(1,2) should pass")
	}
}

func TestNilHandlesTypedNil(t *testing.T) {
	var typedNil *int
	if ok, _ := Nil(typedNil); !ok {
		t.Error("typed nil pointer should count as nil")
	}
	if ok, _ := Nil(any(nil)); !ok {
		t.Error("untyped nil should count as nil")
	}
	if ok, _ := NotNil(0); !ok {
		t.Error("zero int is not nil")
	}
}

func TestZeroLenEmpty(t *testing.T) {
	if ok, _ := Zero(0); !ok {
		t.Error("0 is a zero value")
	}
	if ok, _ := Zero(""); !ok {
		t.Error("empty string is a zero value")
	}
	if ok, _ := Zero(1); ok {
		t.Error("1 is not a zero value")
	}
	if ok, _ := Len([]int{1, 2}, 2); !ok {
		t.Error("Len on slice failed")
	}
	if ok, detail := Len(42, 1); ok || detail == "" {
		t.Error("Len on non-collection must fail with detail")
	}
	value := 3
	if ok, _ := Empty(&value); ok {
		t.Error("pointer to non-zero value is not empty")
	}
	var nilPtr *int
	if ok, _ := Empty(nilPtr); !ok {
		t.Error("nil pointer is empty")
	}
	if ok, _ := NotEmpty([]string{"x"}); !ok {
		t.Error("populated slice is not empty")
	}
}

func TestContainsAcrossContainers(t *testing.T) {
	if ok, _ := Contains("hello world", "world"); !ok {
		t.Error("substring should be contained")
	}
	if ok, _ := Contains([]int{1, 2, 3}, 2); !ok {
		t.Error("slice element should be contained")
	}
	if ok, _ := Contains(map[string]int{"k": 1}, "k"); !ok {
		t.Error("map key should be contained")
	}
	if ok, _ := NotContains([]int{1}, 9); !ok {
		t.Error("absent element should pass NotContains")
	}
	if ok, _ := Contains(42, 4); ok {
		t.Error("int is not a container")
	}
}

func TestErrorHelpers(t *testing.T) {
	base := errors.New("boom")
	wrapped := errors.Join(base, errors.New("ctx"))
	if ok, _ := ErrorIs(wrapped, base); !ok {
		t.Error("wrapped error should match with ErrorIs")
	}
	if ok, _ := ErrorContains(base, "boo"); !ok {
		t.Error("ErrorContains substring should match")
	}
	if ok, _ := ErrorContains(nil, "x"); ok {
		t.Error("nil error never contains anything")
	}
	if ok, _ := NoError(nil); !ok {
		t.Error("NoError(nil) should pass")
	}
	if ok, _ := Error(nil); ok {
		t.Error("Error(nil) should fail")
	}
}

func TestJSONEq(t *testing.T) {
	if ok, _ := JSONEq(`{"a":1,"b":[2,3]}`, `{"b":[2,3],"a":1}`); !ok {
		t.Error("key order must not matter")
	}
	if ok, _ := JSONEq(`{"a":1}`, `{"a":2}`); ok {
		t.Error("different values must not be JSON-equal")
	}
	if ok, _ := JSONEq(`not json`, `{}`); ok {
		t.Error("invalid expected JSON must fail")
	}
}

func TestOrderedComparisons(t *testing.T) {
	if ok, _ := Greater(2, 1); !ok {
		t.Error("2 > 1")
	}
	if ok, _ := Greater(1.5, int64(1)); !ok {
		t.Error("mixed numeric kinds should compare")
	}
	if ok, _ := Less("a", "b"); !ok {
		t.Error("strings should compare lexically")
	}
	if ok, _ := GreaterOrEqual(3, 3); !ok {
		t.Error("3 >= 3")
	}
	if ok, _ := LessOrEqual(4, 3); ok {
		t.Error("4 <= 3 is false")
	}
	earlier := time.Now()
	later := earlier.Add(time.Second)
	if ok, _ := Greater(later, earlier); !ok {
		t.Error("later time should be greater")
	}
	if ok, _ := Greater([]int{1}, []int{0}); ok {
		t.Error("slices are not ordered")
	}
	if ok, _ := Positive(1); !ok {
		t.Error("1 is positive")
	}
	if ok, _ := Positive(0); ok {
		t.Error("0 is not positive")
	}
}

func TestInDeltaSameAndTypes(t *testing.T) {
	if ok, _ := InDelta(1.0, 1.05, 0.1); !ok {
		t.Error("within delta should pass")
	}
	if ok, _ := InDelta(1.0, 2.0, 0.1); ok {
		t.Error("outside delta should fail")
	}
	value := 7
	if ok, _ := Same(&value, &value); !ok {
		t.Error("same pointer should pass")
	}
	other := 7
	if ok, _ := Same(&value, &other); ok {
		t.Error("distinct pointers must fail Same")
	}
	if ok, _ := IsType("", "text"); !ok {
		t.Error("string type should match")
	}
	if ok, _ := IsType(0, "text"); ok {
		t.Error("int vs string types must not match")
	}
	now := time.Now()
	if ok, _ := WithinDuration(now, now.Add(time.Second), 2*time.Second); !ok {
		t.Error("1s difference within 2s delta")
	}
	if ok, _ := WithinDuration(now, now.Add(3*time.Second), time.Second); ok {
		t.Error("3s difference exceeds 1s delta")
	}
}
