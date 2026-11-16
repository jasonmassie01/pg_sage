package workpack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"strings"

	"github.com/pg-sage/sidecar/internal/evidence"
)

// Charts (evid-02c): a 336 x 64 PNG sparkline of each of the top
// ChartFamilies families' hour-of-week exec-time shape and of its weekly
// exec-time growth, rendered with the standard library only. The same
// series always gives the same bytes. A PNG written here carries no text
// chunks, so a chart holds pixels and nothing else. The model and export
// tiers carry them; the stored local tier does not (they are derived), and
// the series themselves stay in workload.json and growth.json as numbers
// for text-only readers.

const (
	chartWidth  = 336
	chartHeight = 64
	chartMargin = 2
	chartDir    = "charts/"
)

var chartPalette = color.Palette{color.White, color.RGBA{0x1f, 0x4e, 0x9e, 0xff}}

// sparkline renders values left to right, min at the bottom and max at
// the top; NaN is a gap. Fewer than two values, or all equal, is a flat
// line across the middle.
func sparkline(values []float64) ([]byte, error) {
	lo, hi, finite := math.Inf(1), math.Inf(-1), 0
	for _, v := range values {
		switch {
		case math.IsInf(v, 0):
			return nil, errors.New("workpack: a chart value is infinite")
		case math.IsNaN(v):
			continue
		}
		lo, hi, finite = math.Min(lo, v), math.Max(hi, v), finite+1
	}
	img := image.NewPaletted(image.Rect(0, 0, chartWidth, chartHeight), chartPalette)
	if finite < 2 || hi == lo {
		drawLine(img, image.Pt(chartMargin, chartHeight/2),
			image.Pt(chartWidth-1-chartMargin, chartHeight/2))
	} else {
		drawSeries(img, values, lo, hi)
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("workpack: encode a chart: %w", err)
	}
	return buf.Bytes(), nil
}

func drawSeries(img *image.Paletted, values []float64, lo, hi float64) {
	span := float64(chartWidth - 1 - 2*chartMargin)
	rows := float64(chartHeight - 1 - 2*chartMargin)
	step := span / float64(max(len(values)-1, 1))
	var prev *image.Point
	for i, v := range values {
		if math.IsNaN(v) {
			prev = nil
			continue
		}
		p := image.Pt(chartMargin+int(math.Round(float64(i)*step)),
			chartMargin+int(math.Round((hi-v)/(hi-lo)*rows)))
		if prev == nil {
			img.SetColorIndex(p.X, p.Y, 1)
		} else {
			drawLine(img, *prev, p)
		}
		prev = &p
	}
}

// drawLine is Bresenham's line from a to b in the ink colour.
func drawLine(img *image.Paletted, a, b image.Point) {
	dx, dy := abs(b.X-a.X), -abs(b.Y-a.Y)
	sx, sy := sign(b.X-a.X), sign(b.Y-a.Y)
	e := dx + dy
	for {
		img.SetColorIndex(a.X, a.Y, 1)
		if a == b {
			return
		}
		if e2 := 2 * e; e2 >= dy {
			e += dy
			a.X += sx
		} else {
			e += dx
			a.Y += sy
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func sign(v int) int {
	switch {
	case v < 0:
		return -1
	case v > 0:
		return 1
	}
	return 0
}

// chartsOf renders p's charts: charts/<family hex>.png (shape) and
// charts/<family hex>-growth.png for the top ChartFamilies families.
func chartsOf(p *Pack) ([]evidence.File, error) {
	var wl Workload
	var g Growth
	if err := json.Unmarshal(p.File(fileWorkload), &wl); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, fileWorkload, err)
	}
	if err := json.Unmarshal(p.File(fileGrowth), &g); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, fileGrowth, err)
	}
	growth := map[string][]*float64{}
	for _, fg := range g.Workload {
		growth[strings.TrimPrefix(fg.ID, "growth/workload/")] = fg.ExecMs.Values
	}
	var out []evidence.File
	for i, f := range wl.Families {
		if i >= ChartFamilies {
			break
		}
		hex := strings.TrimPrefix(f.ID, "workload/family/")
		weekly := make([]float64, len(growth[hex]))
		for j, v := range growth[hex] {
			weekly[j] = math.NaN()
			if v != nil {
				weekly[j] = *v
			}
		}
		for name, series := range map[string][]float64{hex + ".png": f.Shape.ExecMsMean,
			hex + "-growth.png": weekly} {
			data, err := sparkline(series)
			if err != nil {
				return nil, fmt.Errorf("chart %s: %w", name, err)
			}
			out = append(out, evidence.File{Name: chartDir + name, Data: data})
		}
	}
	return out, nil
}
