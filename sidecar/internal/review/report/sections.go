package report

import (
	"embed"
	"fmt"
	"strings"
	"text/template"

	"github.com/pg-sage/sidecar/internal/review/rubric"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

var templates = template.Must(template.ParseFS(templateFS, "templates/*.tmpl"))

type dimensionView struct {
	Title, Letter   string
	Score, Coverage float64
	Candidates      []Ranked
}

// Sections is one templated text per graded dimension, in the grade's
// order, then one per dimension the candidates name that the grade does
// not (in first-ranked order). A nil grade gives one "no grade" section
// before those.
func Sections(g *rubric.Grade, ranked []Ranked) ([]Section, error) {
	byDim, order := byDimension(ranked)
	var out []Section
	graded := map[string]bool{}
	if g == nil {
		text, err := render("nograde", len(ranked))
		if err != nil {
			return nil, err
		}
		out = append(out, Section{Dimension: "overall", Title: "Overall", Text: text,
			Origin: OriginDeterministic})
	} else {
		for _, d := range g.Dimensions {
			graded[d.Dimension] = true
			v := dimensionView{Title: d.Title, Letter: string(d.Letter), Score: d.Score,
				Coverage: d.Coverage, Candidates: byDim[d.Dimension]}
			text, err := render("dimension", v)
			if err != nil {
				return nil, err
			}
			out = append(out, Section{Dimension: d.Dimension, Title: d.Title,
				Letter: string(d.Letter), Text: text, Origin: OriginDeterministic})
		}
	}
	for _, dim := range order {
		if graded[dim] {
			continue
		}
		text, err := render("ungraded", dimensionView{Title: dim, Candidates: byDim[dim]})
		if err != nil {
			return nil, err
		}
		out = append(out, Section{Dimension: dim, Title: dim, Text: text,
			Origin: OriginDeterministic})
	}
	return out, nil
}

func byDimension(ranked []Ranked) (map[string][]Ranked, []string) {
	byDim := map[string][]Ranked{}
	var order []string
	for _, r := range ranked {
		if _, ok := byDim[r.Dimension]; !ok {
			order = append(order, r.Dimension)
		}
		byDim[r.Dimension] = append(byDim[r.Dimension], r)
	}
	return byDim, order
}

func render(name string, data any) (string, error) {
	var b strings.Builder
	if err := templates.ExecuteTemplate(&b, name, data); err != nil {
		return "", fmt.Errorf("render the review report's %s text: %w", name, err)
	}
	return strings.TrimSpace(b.String()), nil
}
