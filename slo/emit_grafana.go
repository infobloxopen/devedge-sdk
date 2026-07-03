package slo

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Grafana dashboard model (portable JSON, importable into any Grafana; the
// internal overlay wraps this same JSON in a GrafanaDashboard CR). Ordered
// structs give deterministic output.
type grafanaDashboard struct {
	Title         string         `json:"title"`
	UID           string         `json:"uid"`
	Editable      bool           `json:"editable"`
	SchemaVersion int            `json:"schemaVersion"`
	Tags          []string       `json:"tags"`
	Time          grafanaTime    `json:"time"`
	Templating    grafanaTmpl    `json:"templating"`
	Panels        []grafanaPanel `json:"panels"`
	Annotations   grafanaAnnots  `json:"annotations"`
}

type grafanaTime struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type grafanaTmpl struct {
	List []grafanaVar `json:"list"`
}

type grafanaVar struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Query string `json:"query"`
	Label string `json:"label"`
}

type grafanaAnnots struct {
	List []any `json:"list"`
}

type grafanaPanel struct {
	ID         int             `json:"id"`
	Title      string          `json:"title"`
	Type       string          `json:"type"`
	Datasource grafanaDS       `json:"datasource"`
	GridPos    grafanaGrid     `json:"gridPos"`
	Targets    []grafanaTarget `json:"targets"`
}

type grafanaDS struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

type grafanaGrid struct {
	H int `json:"h"`
	W int `json:"w"`
	X int `json:"x"`
	Y int `json:"y"`
}

type grafanaTarget struct {
	Expr         string `json:"expr"`
	LegendFormat string `json:"legendFormat"`
	RefID        string `json:"refId"`
}

// emitGrafana projects the Document to one importable Grafana dashboard JSON per
// service: an SLI-trend, an error-budget burndown, and a burn-rate panel per SLO.
func emitGrafana(doc *Document, naming MetricNaming) ([]Rendered, error) {
	// Group SLOs by service, deterministically.
	byService := map[string][]*SLO{}
	var order []string
	for i := range doc.SLOs {
		s := &doc.SLOs[i]
		if _, ok := byService[s.Spec.Service]; !ok {
			order = append(order, s.Spec.Service)
		}
		byService[s.Spec.Service] = append(byService[s.Spec.Service], s)
	}
	sort.Strings(order)

	var out []Rendered
	for _, svc := range order {
		dash := grafanaDashboard{
			Title:         svc + " SLOs",
			UID:           svc + "-slo",
			Editable:      true,
			SchemaVersion: 39,
			Tags:          []string{"devedge", "slo"},
			Time:          grafanaTime{From: "now-28d", To: "now"},
			Templating: grafanaTmpl{List: []grafanaVar{
				{Name: "datasource", Type: "datasource", Query: "prometheus", Label: "Data source"},
			}},
			Annotations: grafanaAnnots{List: []any{}},
		}
		ds := grafanaDS{Type: "prometheus", UID: "${datasource}"}
		id := 0
		y := 0
		for _, s := range byService[svc] {
			sli := doc.sliByName(s.Spec.IndicatorRef)
			if sli == nil && s.Spec.Indicator != nil {
				sli = s.Spec.Indicator
			}
			if sli == nil || sli.Spec.RatioMetric == nil {
				return nil, fmt.Errorf("slo: emit grafana: SLO %q has no resolvable ratio indicator", s.Metadata.Name)
			}
			if len(s.Spec.Objectives) == 0 {
				return nil, fmt.Errorf("slo: emit grafana: SLO %q has no objective", s.Metadata.Name)
			}
			target := s.Spec.Objectives[0].Target
			rm := sli.Spec.RatioMetric
			budget := "(1 - " + formatFloat(target) + ")"
			window := "28d"
			if len(s.Spec.TimeWindow) > 0 {
				window = s.Spec.TimeWindow[0].Duration
			}
			errRatio1h, err := errorRatioExpr(naming, rm, "1h")
			if err != nil {
				return nil, fmt.Errorf("slo: emit grafana: SLO %q: %w", s.Metadata.Name, err)
			}
			errRatioWin, err := errorRatioExpr(naming, rm, window)
			if err != nil {
				return nil, fmt.Errorf("slo: emit grafana: SLO %q: %w", s.Metadata.Name, err)
			}

			id++
			dash.Panels = append(dash.Panels, grafanaPanel{
				ID: id, Title: s.Metadata.Name + " — error rate (1h) vs budget", Type: "timeseries",
				Datasource: ds, GridPos: grafanaGrid{H: 8, W: 8, X: 0, Y: y},
				Targets: []grafanaTarget{
					{Expr: errRatio1h, LegendFormat: "error rate (1h)", RefID: "A"},
					{Expr: budget, LegendFormat: "budget", RefID: "B"},
				},
			})
			id++
			dash.Panels = append(dash.Panels, grafanaPanel{
				ID: id, Title: s.Metadata.Name + " — error-budget burndown (" + window + ")", Type: "timeseries",
				Datasource: ds, GridPos: grafanaGrid{H: 8, W: 8, X: 8, Y: y},
				Targets: []grafanaTarget{
					{Expr: "1 - ((" + errRatioWin + ") / " + budget + ")", LegendFormat: "budget remaining", RefID: "A"},
				},
			})
			id++
			dash.Panels = append(dash.Panels, grafanaPanel{
				ID: id, Title: s.Metadata.Name + " — burn rate (1h)", Type: "timeseries",
				Datasource: ds, GridPos: grafanaGrid{H: 8, W: 8, X: 16, Y: y},
				Targets: []grafanaTarget{
					{Expr: "(" + errRatio1h + ") / " + budget, LegendFormat: "burn rate", RefID: "A"},
				},
			})
			y += 8
		}
		b, err := json.MarshalIndent(&dash, "", "  ")
		if err != nil {
			return nil, err
		}
		b = append(b, '\n')
		out = append(out, Rendered{Filename: svc + "-slo.dashboard.json", Content: b})
	}
	return out, nil
}
