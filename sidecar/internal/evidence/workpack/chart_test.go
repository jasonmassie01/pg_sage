package workpack

import (
	"bytes"
	"image"
	"image/png"
	"math"
	"strings"
	"testing"
	"time"
)

// evid-02c charts: a 336 x 64 PNG sparkline per top family's hour-of-week
// shape and weekly growth, deterministic for the same data.
// No concurrency test here: rendering is a pure function of its input.

// goldenSparklineSHA256 pins the bytes of sparkline(goldenSeries()), taken
// from the first render (Go 1.27 image/png, BestCompression).
const goldenSparklineSHA256 = "63d19e6f44cfd50084e37096ccb320dbce5605d2f2246da52315074c1cd2c86f"

func goldenSeries() []float64 {
	out := make([]float64, 168)
	for i := range out {
		out[i] = 10 + 8*math.Sin(float64(i)/168*2*math.Pi*7)
	}
	return out
}

func decodePNG(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode the chart: %v", err)
	}
	return img
}

func inked(img image.Image, x, y int) bool {
	r, g, b, _ := img.At(x, y).RGBA()
	return r != 0xffff || g != 0xffff || b != 0xffff
}

func TestSparkline_SizeAndGoldenBytes(t *testing.T) {
	data, err := sparkline(goldenSeries())
	if err != nil {
		t.Fatal(err)
	}
	img := decodePNG(t, data)
	if b := img.Bounds(); b.Dx() != chartWidth || b.Dy() != chartHeight ||
		chartWidth != 336 || chartHeight != 64 {
		t.Fatalf("chart is %v", b)
	}
	again, _ := sparkline(goldenSeries())
	if !bytes.Equal(data, again) {
		t.Fatal("the same series rendered two different PNGs")
	}
	if got := sha256Hex(data); got != goldenSparklineSHA256 {
		t.Fatalf("sparkline PNG sha256 = %s, want %s", got, goldenSparklineSHA256)
	}
	// The curve spans the plot: ink near the top and near the bottom.
	top, bottom := false, false
	for x := 0; x < chartWidth; x++ {
		top = top || inked(img, x, chartMargin)
		bottom = bottom || inked(img, x, chartHeight-1-chartMargin)
	}
	if !top || !bottom {
		t.Fatalf("the curve does not span the plot: top %v bottom %v", top, bottom)
	}
}

func TestSparkline_AllZeroIsAFlatLine(t *testing.T) {
	for _, series := range [][]float64{make([]float64, 168), nil, {5},
		{math.NaN(), math.NaN()}} {
		data, err := sparkline(series)
		if err != nil {
			t.Fatalf("%v: %v", series, err)
		}
		img := decodePNG(t, data)
		mid := chartHeight / 2
		for y := 0; y < chartHeight; y++ {
			for x := chartMargin; x < chartWidth-chartMargin; x++ {
				if inked(img, x, y) != (y == mid) {
					t.Fatalf("%v: pixel %d,%d inked %v, want a flat line at y=%d", series,
						x, y, inked(img, x, y), mid)
				}
			}
		}
	}
}

func TestSparkline_InfiniteValuesAreRefused(t *testing.T) {
	if _, err := sparkline([]float64{1, math.Inf(1)}); err == nil {
		t.Fatal("an infinite value rendered")
	}
}

func TestCharts_TopTenFamiliesShapeAndGrowth(t *testing.T) {
	w := fourWeeks()
	m := mustForModel(t, mustAssemble(t, synthRequest(w), manyFamilies(w, 12)), ModelOptions{})
	wl := workloadOf(t, m)
	want := map[string]bool{}
	for i, f := range wl.Families {
		if i >= ChartFamilies {
			break
		}
		hex := strings.TrimPrefix(f.ID, "workload/family/")
		want[chartDir+hex+".png"], want[chartDir+hex+"-growth.png"] = true, true
	}
	got := map[string]bool{}
	for _, f := range m.Files {
		if strings.HasPrefix(f.Name, chartDir) {
			got[f.Name] = true
			decodePNG(t, f.Data)
		}
	}
	if len(got) != 2*ChartFamilies || len(want) != 2*ChartFamilies {
		t.Fatalf("charts %d (want %d)", len(got), 2*ChartFamilies)
	}
	for name := range want {
		if !got[name] {
			t.Errorf("chart %s missing", name)
		}
	}
}

func TestCharts_BuiltTwiceHaveTheSameHashes(t *testing.T) {
	w := Window{monday, monday.Add(28 * 24 * time.Hour)}
	a := mustForModel(t, mustAssemble(t, synthRequest(w), synthHistory(w)), ModelOptions{})
	b := mustForModel(t, mustAssemble(t, synthRequest(w), synthHistory(w)), ModelOptions{})
	n := 0
	for _, f := range a.Files {
		if strings.HasPrefix(f.Name, chartDir) {
			n++
			if !bytes.Equal(f.Data, b.File(f.Name)) {
				t.Errorf("chart %s differs between two builds", f.Name)
			}
		}
	}
	if n == 0 {
		t.Fatal("no charts")
	}
}
