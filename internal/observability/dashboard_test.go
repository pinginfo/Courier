package observability

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The Grafana dashboard shipped in the chart must be valid JSON and must only
// query metrics Courier (or the standard Go/process collectors) really exports.
func TestDashboardQueriesExistingMetrics(t *testing.T) {
	raw, err := os.ReadFile("../../charts/courier/dashboards/courier.json")
	if err != nil {
		t.Fatal(err)
	}
	var dash struct {
		UID    string `json:"uid"`
		Panels []panel
	}
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatalf("dashboard is not valid JSON: %v", err)
	}
	if dash.UID == "" {
		t.Error("dashboard has no uid")
	}

	// Names Prometheus exposes for the registered collectors.
	m := NewMetrics("test")
	m.InitRepo("r")
	m.SyncDuration.WithLabelValues("r").Observe(1)
	m.UpstreamDuration.WithLabelValues("r", "200").Observe(1)
	m.S3Duration.WithLabelValues("put", "ok").Observe(1)
	m.Files.WithLabelValues("r", "mirrored").Set(1)
	m.Bytes.WithLabelValues("r", "downloaded").Add(1)
	m.Errors.WithLabelValues("r", "plan", "provider").Inc()
	m.GCDeletedObjects.WithLabelValues("r").Add(1)
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, f := range families {
		known[f.GetName()] = true
		if f.GetType().String() == "HISTOGRAM" {
			for _, s := range []string{"_bucket", "_sum", "_count"} {
				known[f.GetName()+s] = true
			}
		}
	}

	metricRe := regexp.MustCompile(`\b(courier_[a-z0-9_]+|go_[a-z0-9_]+|process_[a-z0-9_]+)\b`)
	ids := map[int]bool{}
	var walk func(ps []panel)
	queries := 0
	walk = func(ps []panel) {
		for _, p := range ps {
			if ids[p.ID] {
				t.Errorf("duplicate panel id %d", p.ID)
			}
			ids[p.ID] = true
			for _, tg := range p.Targets {
				queries++
				for _, name := range metricRe.FindAllString(tg.Expr, -1) {
					if !known[name] {
						t.Errorf("panel %q queries unknown metric %q", p.Title, name)
					}
				}
				if strings.Contains(tg.Expr, "courier_") && !strings.Contains(tg.Expr, `job=~"$job"`) {
					t.Errorf("panel %q does not filter by $job", p.Title)
				}
			}
			walk(p.Panels)
		}
	}
	walk(dash.Panels)
	if queries < 15 {
		t.Errorf("only %d queries found; is the dashboard empty?", queries)
	}
}

type panel struct {
	ID      int      `json:"id"`
	Title   string   `json:"title"`
	Targets []target `json:"targets"`
	Panels  []panel  `json:"panels"`
}

type target struct {
	Expr string `json:"expr"`
}
