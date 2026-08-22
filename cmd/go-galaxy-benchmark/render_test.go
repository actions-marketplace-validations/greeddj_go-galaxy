package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAxisMax(t *testing.T) {
	cases := []struct {
		name string
		peak float64
		want float64
	}{
		{name: "no rows", peak: 0, want: 1},
		{name: "just under a decade", peak: 9.6, want: 10},
		{name: "needs headroom above the peak", peak: 21.1, want: 25},
		{name: "sits on a step", peak: 2, want: 2.5},
		{name: "three digits", peak: 330.1, want: 500},
		{name: "tiny", peak: 0.4, want: 0.5},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := []chartRow{}
			if tc.peak > 0 {
				rows = append(rows, chartRow{value: tc.peak})
			}

			if got := axisMax(rows); got != tc.want {
				t.Fatalf("axisMax(%v) = %v, want %v", tc.peak, got, tc.want)
			}
		})
	}
}

func TestAxisMaxAlwaysLeavesTheLongestBarRoom(t *testing.T) {
	for _, peak := range []float64{1, 3.1, 19.8, 21.1, 26.4, 125.6, 330.1, 1000} {
		rows := []chartRow{{value: peak}}
		if got := axisMax(rows); got < peak {
			t.Fatalf("axisMax(%v) = %v, which would clip the bar", peak, got)
		}
	}
}

func TestTrimFloat(t *testing.T) {
	cases := map[float64]string{25: "25", 3.1: "3.1", 3.14159: "3.14", 500: "500", 0.5: "0.5"}
	for value, want := range cases {
		if got := trimFloat(value); got != want {
			t.Fatalf("trimFloat(%v) = %q, want %q", value, got, want)
		}
	}
}

func TestShortVersion(t *testing.T) {
	cases := map[string]string{
		"ansible-galaxy [core 2.20.5]":                    "2.20.5",
		"v1.0.2 (built by go) // go1.27.0":                "v1.0.2",
		"v1.0.3-0.20260822031409-58c23c37177d (commit x)": "v1.0.3-0.20260822031409-58c23c37177d",
		"unknown": "unknown",
	}

	for line, want := range cases {
		if got := shortVersion(line); got != want {
			t.Fatalf("shortVersion(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestCollectionCount(t *testing.T) {
	if got := collectionCount(1); got != "1 collection" {
		t.Fatalf("collectionCount(1) = %q", got)
	}

	if got := collectionCount(10); got != "10 collections" {
		t.Fatalf("collectionCount(10) = %q", got)
	}
}

func TestSpeedupRefusesToDivideByAMissingMeasurement(t *testing.T) {
	report := sampleReport()

	ratio, ok := speedup(report, scenarioCold, 1)
	if !ok {
		t.Fatal("speedup reported nothing for a cell both tools measured")
	}

	if ratio < 3 || ratio > 3.1 {
		t.Fatalf("speedup = %v, want about 3.06", ratio)
	}

	// A series where every run failed carries no samples. Treating that as
	// zero seconds would print an infinite speedup.
	report.Results = append(report.Results,
		Result{Scenario: scenarioWarm, Tool: "ansible-galaxy", Size: 1, SamplesMS: []int64{5000}},
		Result{Scenario: scenarioWarm, Tool: "go-galaxy", Size: 1, Failed: 3},
	)

	if _, ok := speedup(report, scenarioWarm, 1); ok {
		t.Fatal("speedup produced a ratio against a series with no successful run")
	}

	if _, ok := speedup(report, scenarioCold, 999); ok {
		t.Fatal("speedup produced a ratio for a size nobody measured")
	}
}

func TestRenderTableGolden(t *testing.T) {
	var out strings.Builder
	if err := renderTable(&out, sampleReport()); err != nil {
		t.Fatalf("renderTable: %v", err)
	}

	got := out.String()

	// The provenance block names the tool and then quotes its --version line
	// verbatim, which is why the first field repeats.
	for _, want := range []string{
		"ansible-galaxy  ansible-galaxy [core 2.20.5]", //nolint:dupword
		"host            linux/amd64, 8 cpus, xfs",
		"measurement     2 runs, dependencies resolved",
		"SCENARIO  SIZE  TOOL            MEAN    MIN     MAX     FAILED",
		"cold      1     ansible-galaxy  8.990s  8.980s  9.000s  0",
		"cold      1     go-galaxy       2.930s  2.860s  3.000s  0",
		"cold      1     speedup         3.1x",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("renderTable output missing %q:\n%s", want, got)
		}
	}
}

func TestRenderTableSaysWhenNothingSucceeded(t *testing.T) {
	report := sampleReport()
	report.Results = append(report.Results,
		Result{Scenario: scenarioWarm, Tool: "go-galaxy", Size: 1, Failed: 5})

	var out strings.Builder
	if err := renderTable(&out, report); err != nil {
		t.Fatalf("renderTable: %v", err)
	}

	if !strings.Contains(out.String(), "no successful run") {
		t.Fatalf("renderTable hid a series that never succeeded:\n%s", out.String())
	}
}

// TestEmitSVGCarriesTheMeasuredValueVerbatim guards the property the whole
// chart emitter is built around: a bar's width attribute is the number that
// was measured, and the axis is the viewBox width. Nothing in the emitter
// converts a value into a coordinate, so nothing can convert it wrongly.
func TestEmitSVGCarriesTheMeasuredValueVerbatim(t *testing.T) {
	panel := chartPanel{title: "Cold cache", rows: []chartRow{
		{label: "10 collections", detail: "152.28s -> 7.71s", value: 19.8},
		{label: "100 collections", detail: "458.10s -> 21.67s", value: 21.1},
	}}

	svg := emitSVG("title", "subtitle", []chartPanel{panel})

	for _, want := range []string{
		`viewBox="0 0 25 64" preserveAspectRatio="none"`,
		`<rect class="bar" y="0" height="18" width="19.8"/>`,
		`<rect class="bar" y="32" height="18" width="21.1"/>`,
	} {
		if !strings.Contains(svg, want) {
			t.Fatalf("SVG missing %q:\n%s", want, svg)
		}
	}
}

func TestEmitSVGEscapesWhatItDidNotAuthor(t *testing.T) {
	panel := chartPanel{title: "Cold cache", rows: []chartRow{
		{label: "1 collection", detail: "a & b <script>", value: 2},
	}}

	svg := emitSVG("t", `go-galaxy "v1" & co`, []chartPanel{panel})

	if strings.Contains(svg, "<script>") {
		t.Fatalf("SVG carries an unescaped tag:\n%s", svg)
	}

	if !strings.Contains(svg, "&amp;") {
		t.Fatalf("SVG did not escape an ampersand:\n%s", svg)
	}
}

func TestBuildPanelsFiltersByScenario(t *testing.T) {
	report := sampleReport()
	report.Results = append(report.Results,
		Result{Scenario: scenarioWarm, Tool: "ansible-galaxy", Size: 1, SamplesMS: []int64{5000}},
		Result{Scenario: scenarioWarm, Tool: "go-galaxy", Size: 1, SamplesMS: []int64{1000}},
	)

	if got := buildPanels(report, ""); len(got) != 2 {
		t.Fatalf("buildPanels(all) returned %d panels, want 2", len(got))
	}

	got := buildPanels(report, scenarioWarm)
	if len(got) != 1 || got[0].title != "Warm cache" {
		t.Fatalf("buildPanels(warm) = %+v, want one Warm cache panel", got)
	}
}

func TestWriteSVGCreatesItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "charts", "bench.svg")
	if err := writeSVG(sampleReport(), "", path); err != nil {
		t.Fatalf("writeSVG: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("chart was not written: %v", err)
	}
}

func TestWriteSVGRefusesAScenarioWithNothingInIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bench.svg")
	if err := writeSVG(sampleReport(), scenarioWarm, path); err == nil {
		t.Fatal("writeSVG rendered a chart for a scenario the report does not carry")
	}
}
