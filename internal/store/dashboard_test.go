package store_test

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/mp2209/pitwall/internal/store/storetest"
)

// Every SQL query in the Grafana dashboard must run against the current
// schema, so a migration can't silently break a panel.
func TestDashboardQueriesRun(t *testing.T) {
	st := storetest.New(t)
	raw, err := os.ReadFile("../../deploy/grafana/dashboards/pitwall.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Templating struct {
			List []struct {
				Name  string `json:"name"`
				Query string `json:"query"`
			} `json:"list"`
		} `json:"templating"`
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				RawSQL string `json:"rawSql"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	timeFilter := regexp.MustCompile(`\$__timeFilter\(([^)]+)\)`)
	run := func(name, q string) {
		q = timeFilter.ReplaceAllString(q, "$1 BETWEEN now() - interval '1 day' AND now()")
		q = strings.ReplaceAll(q, "$task", "t_none")
		if strings.Contains(q, "$") {
			t.Errorf("%s: unhandled Grafana macro in %q", name, q)
			return
		}
		if err := st.Exec(ctx, q); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	n := 0
	for _, v := range d.Templating.List {
		run("variable "+v.Name, v.Query)
		n++
	}
	for _, p := range d.Panels {
		for _, tg := range p.Targets {
			if tg.RawSQL != "" {
				run(p.Title, tg.RawSQL)
				n++
			}
		}
	}
	if n < 8 {
		t.Errorf("only %d queries found; is the dashboard file complete?", n)
	}
}
