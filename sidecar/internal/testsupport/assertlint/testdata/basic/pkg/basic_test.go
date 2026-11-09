package basic

import (
	"errors"
	"sync"
	"testing"

	"example.com/fix/helpers"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestMain(m *testing.M) { m.Run() }

func Testlower(t *testing.T) {}

func TestFatal(t *testing.T) {
	if 1+1 != 2 {
		t.Fatalf("math")
	}
}

func TestNoCall(t *testing.T) {
	x := 1
	_ = x
}

func TestEmpty(t *testing.T) {}

func TestErrString(t *testing.T) {
	_ = errors.New("x").Error()
}

func TestRequire(t *testing.T) {
	require.Equal(t, 1, 1)
}

func TestSubtestFails(t *testing.T) {
	t.Run("a", func(t *testing.T) { t.Error("x") })
}

func TestSubtestHelperValue(t *testing.T) {
	t.Run("a", mustFail)
}

func TestSubtestHollow(t *testing.T) {
	t.Run("a", func(t *testing.T) { t.Log("x") })
}

func TestFailingHelperChain(t *testing.T) {
	outer(t)
}

func TestQuietHelper(t *testing.T) {
	quiet(t)
}

func TestExternalFails(t *testing.T) {
	helpers.Must(t)
}

func TestExternalQuiet(t *testing.T) {
	helpers.Soft(t)
}

func TestFieldReceiver(t *testing.T) {
	h := harness{t: t}
	h.check()
}

func TestRaceWithWait(t *testing.T) {
	var wg sync.WaitGroup
	n := 0
	var mu sync.Mutex
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			n++
			mu.Unlock()
		}()
	}
	wg.Wait()
}

func TestGoroutineNoWait(t *testing.T) {
	done := make(chan struct{})
	go func() { close(done) }()
}

func mustFail(t *testing.T) { t.FailNow() }

// outer fails only through inner, declared after it.
func outer(t *testing.T) { inner(t) }

func inner(t testing.TB) {
	t.Helper()
	t.Fatal("inner")
}

func quiet(t *testing.T) {
	t.Helper()
	t.Log("ok")
}

type harness struct{ t *testing.T }

func (h harness) check() { h.t.Errorf("x") }
