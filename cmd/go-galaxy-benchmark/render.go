package main

import (
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

// Table layout. tabwriter needs a minimum cell width, padding and a pad
// character; these three are the whole configuration.
const (
	tableMinWidth = 2
	tableTabWidth = 2
	tablePadding  = 2
)

// Chart layout, in user units of the outer viewBox. Named rather than
// inlined so the emitter carries no bare geometry and reshaping the chart is
// a change to one block.
const (
	canvasWidth   = 900
	labelColumnX  = 158
	barColumnX    = 170
	barColumnW    = 420
	valueColumnX  = 606
	rowPitch      = 32
	barHeight     = 18
	panelHeadRoom = 30
	panelGap      = 22
	topMatter     = 60
	bottomMatter  = 16
	baselineDrop  = 13
	titleDrop     = 22
	subtitleDrop  = 42
	headingLift   = 9
	axisHeadroom  = 1.04
	fileMode      = 0o644
	decadeStep    = 10
)

// tableWriter records the first write error so the row emitters below stay
// straight-line code. tabwriter buffers anyway, so a real I/O failure lands
// at Flush rather than at any one Fprintf.
type tableWriter struct {
	w   io.Writer
	err error
}

// printf writes one row unless an earlier write already failed.
func (t *tableWriter) printf(format string, args ...any) {
	if t.err != nil {
		return
	}

	_, t.err = fmt.Fprintf(t.w, format, args...)
}

// renderTable writes the report as a plain text table. Every scenario and
// size gets a speedup row, because the ratio is the number the whole exercise
// exists to produce and reading it off two adjacent means is needless work.
func renderTable(w io.Writer, report *Report) error {
	if err := renderHeader(w, report); err != nil {
		return err
	}

	aligned := tabwriter.NewWriter(w, tableMinWidth, tableTabWidth, tablePadding, ' ', 0)
	table := &tableWriter{w: aligned}

	table.printf("SCENARIO\tSIZE\tTOOL\tMEAN\tMIN\tMAX\tFAILED\n")

	for _, scenario := range report.scenarios() {
		for _, size := range report.sizes() {
			renderRows(table, report, scenario, size)
		}
	}

	if table.err != nil {
		return fmt.Errorf("writing table: %w", table.err)
	}

	if err := aligned.Flush(); err != nil {
		return fmt.Errorf("flushing table: %w", err)
	}

	return nil
}

// renderHeader writes the provenance block above the table: which builds were
// measured, on what, and under which flags.
func renderHeader(w io.Writer, report *Report) error {
	deps := "--no-deps"
	if report.ResolveDeps {
		deps = "dependencies resolved"
	}

	_, err := fmt.Fprintf(w,
		"ansible-galaxy  %s\ngo-galaxy       %s\nhost            %s/%s, %d cpus, %s\nmeasurement     %d runs, %s\n\n",
		report.Tools["ansible-galaxy"].Version,
		report.Tools["go-galaxy"].Version,
		report.Host.OS, report.Host.Arch, report.Host.CPUs, report.Host.Filesystem,
		report.Runs, deps,
	)
	if err != nil {
		return fmt.Errorf("writing report header: %w", err)
	}

	return nil
}

// renderRows writes both tools' rows for one scenario and size, followed by
// their ratio.
func renderRows(w *tableWriter, report *Report, scenario string, size int) {
	for _, tool := range []string{"ansible-galaxy", "go-galaxy"} {
		result, ok := report.find(scenario, size, tool)
		if !ok {
			continue
		}

		stat := result.stats()
		if stat.Count == 0 {
			w.printf("%s\t%d\t%s\tno successful run\t\t\t%d\n", scenario, size, tool, result.Failed)

			continue
		}

		w.printf("%s\t%d\t%s\t%s\t%s\t%s\t%d\n",
			scenario, size, tool,
			milliseconds(stat.MeanMS),
			milliseconds(float64(stat.MinMS)),
			milliseconds(float64(stat.MaxMS)),
			result.Failed)
	}

	if ratio, ok := speedup(report, scenario, size); ok {
		w.printf("%s\t%d\tspeedup\t%.1fx\t\t\t\n", scenario, size, ratio)
	}
}

// speedup is how many times faster go-galaxy was. It reports false when
// either side has no successful run, so a missing measurement never becomes a
// ratio against zero.
func speedup(report *Report, scenario string, size int) (float64, bool) {
	slow, okSlow := report.find(scenario, size, "ansible-galaxy")
	fast, okFast := report.find(scenario, size, "go-galaxy")

	if !okSlow || !okFast {
		return 0, false
	}

	slowStat, fastStat := slow.stats(), fast.stats()
	if slowStat.Count == 0 || fastStat.Count == 0 || fastStat.MeanMS <= 0 {
		return 0, false
	}

	return slowStat.MeanMS / fastStat.MeanMS, true
}

// chartRow is one bar: how many times faster, and the absolute pair the ratio
// came from.
type chartRow struct {
	label  string
	detail string
	value  float64
}

// chartPanel is one scenario's worth of bars.
type chartPanel struct {
	title string
	rows  []chartRow
}

// writeSVG renders the report as a bar chart and writes it to path.
//
// The bars carry the ratio rather than the elapsed time. Seconds cannot share
// a linear axis here: a warm run separates the two tools by three orders of
// magnitude, and the faster bar would be narrower than a pixel. The absolute
// pair moves into the row's text instead, where it is still readable.
func writeSVG(report *Report, scenario, path string) error {
	panels := buildPanels(report, scenario)
	if len(panels) == 0 {
		return fmt.Errorf("%w: nothing to chart for scenario %q", errReportEmpty, scenario)
	}

	subtitle := fmt.Sprintf("ansible-galaxy %s / go-galaxy %s - %s/%s, %s, %d runs",
		shortVersion(report.Tools["ansible-galaxy"].Version),
		shortVersion(report.Tools["go-galaxy"].Version),
		report.Host.OS, report.Host.Arch, report.Host.Filesystem, report.Runs)

	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return fmt.Errorf("creating chart directory: %w", err)
	}

	svg := emitSVG("go-galaxy vs ansible-galaxy - install speedup", subtitle, panels)

	if err := os.WriteFile(path, []byte(svg), fileMode); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	return nil
}

// buildPanels turns the report into one panel per scenario, skipping any
// scenario the caller filtered out and any row without a ratio.
func buildPanels(report *Report, scenario string) []chartPanel {
	panels := make([]chartPanel, 0, len(report.scenarios()))

	for _, name := range report.scenarios() {
		if scenario != "" && name != scenario {
			continue
		}

		panel := chartPanel{title: strings.ToUpper(name[:1]) + name[1:] + " cache"}

		for _, size := range report.sizes() {
			ratio, ok := speedup(report, name, size)
			if !ok {
				continue
			}

			panel.rows = append(panel.rows, chartRow{
				label:  collectionCount(size),
				detail: chartDetail(report, name, size),
				value:  ratio,
			})
		}

		if len(panel.rows) > 0 {
			panels = append(panels, panel)
		}
	}

	return panels
}

// chartDetail renders the absolute pair a ratio came from.
func chartDetail(report *Report, scenario string, size int) string {
	slow, _ := report.find(scenario, size, "ansible-galaxy")
	fast, _ := report.find(scenario, size, "go-galaxy")

	return milliseconds(slow.stats().MeanMS) + " -> " + milliseconds(fast.stats().MeanMS)
}

// axisMax rounds up to the next 1, 2, 2.5 or 5 times a power of ten, so the
// longest bar stops short of the panel edge without anyone choosing a scale.
func axisMax(rows []chartRow) float64 {
	peak := 0.0
	for _, row := range rows {
		if row.value > peak {
			peak = row.value
		}
	}

	if peak <= 0 {
		return 1
	}

	magnitude := 1.0
	for magnitude*decadeStep <= peak {
		magnitude *= decadeStep
	}

	for magnitude > peak {
		magnitude /= decadeStep
	}

	for _, step := range []float64{1, 2, 2.5, 5, decadeStep} {
		if candidate := step * magnitude; candidate >= peak*axisHeadroom {
			return candidate
		}
	}

	return magnitude * decadeStep
}

// emitSVG writes the document. Bars live inside a nested <svg> whose viewBox
// is expressed in data units and whose preserveAspectRatio is none, so a
// bar's width attribute is the ratio verbatim and the renderer does the
// scaling. Nothing here computes a bar coordinate. Text stays outside that
// element: the horizontal scaling would stretch it.
func emitSVG(title, subtitle string, panels []chartPanel) string {
	height := topMatter + bottomMatter
	for _, panel := range panels {
		height += panelHeadRoom + len(panel.rows)*rowPitch + panelGap
	}

	var out strings.Builder

	writeSVGHead(&out, title, subtitle, height)

	y := topMatter
	for _, panel := range panels {
		y = writeSVGPanel(&out, panel, y)
	}

	out.WriteString("</svg>\n")

	return out.String()
}

// writeSVGHead opens the document and writes the two heading lines.
func writeSVGHead(out *strings.Builder, title, subtitle string, height int) {
	fmt.Fprintf(out, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d"`+
		` font-family="-apple-system, BlinkMacSystemFont, 'Segoe UI', Helvetica, Arial, sans-serif">`+"\n",
		canvasWidth, height, canvasWidth, height)
	fmt.Fprintf(out, "  <title>%s</title>\n", html.EscapeString(title))
	out.WriteString("  <style>\n" +
		"    .h { fill: #6b7280; font-size: 15px; font-weight: 600; }\n" +
		"    .t { fill: #6b7280; font-size: 13px; }\n" +
		"    .n { fill: #6b7280; font-size: 13px; font-weight: 600; }\n" +
		"    .d { fill: #9aa1ab; font-size: 12px; }\n" +
		"    .ax { stroke: #9aa1ab; stroke-width: 1; }\n" +
		"    .bar { fill: #3a8fd4; }\n" +
		"  </style>\n")
	fmt.Fprintf(out, "  <text class=\"h\" x=\"8\" y=\"%d\">%s</text>\n", titleDrop, html.EscapeString(title))
	fmt.Fprintf(out, "  <text class=\"d\" x=\"8\" y=\"%d\">%s</text>\n", subtitleDrop, html.EscapeString(subtitle))
}

// writeSVGPanel writes one panel and returns the y the next one starts at.
func writeSVGPanel(out *strings.Builder, panel chartPanel, y int) int {
	maximum := axisMax(panel.rows)
	barsY := y + panelHeadRoom
	barsH := len(panel.rows) * rowPitch

	fmt.Fprintf(out, "\n  <text class=\"h\" x=\"8\" y=\"%d\">%s</text>\n",
		y+baselineDrop+headingLift, html.EscapeString(panel.title))
	fmt.Fprintf(out, "  <line class=\"ax\" x1=\"%d\" y1=\"%d\" x2=\"%d\" y2=\"%d\"/>\n",
		barColumnX, barsY, barColumnX, barsY+barsH-(rowPitch-barHeight))
	fmt.Fprintf(out, "  <svg x=\"%d\" y=\"%d\" width=\"%d\" height=\"%d\" viewBox=\"0 0 %s %d\""+
		" preserveAspectRatio=\"none\">\n", barColumnX, barsY, barColumnW, barsH, trimFloat(maximum), barsH)

	for i, row := range panel.rows {
		fmt.Fprintf(out, "    <rect class=\"bar\" y=\"%d\" height=\"%d\" width=\"%s\"/>\n",
			i*rowPitch, barHeight, trimFloat(row.value))
	}

	out.WriteString("  </svg>\n")

	for i, row := range panel.rows {
		textY := barsY + i*rowPitch + baselineDrop
		fmt.Fprintf(out, "  <text class=\"t\" x=\"%d\" y=\"%d\" text-anchor=\"end\">%s</text>\n",
			labelColumnX, textY, html.EscapeString(row.label))
		fmt.Fprintf(out, "  <text class=\"n\" x=\"%d\" y=\"%d\">%sx <tspan class=\"d\">%s</tspan></text>\n",
			valueColumnX, textY, trimFloat(row.value), html.EscapeString(row.detail))
	}

	return barsY + barsH + panelGap
}

// shortVersion reduces a --version line to what a caption can carry.
// ansible-galaxy answers "ansible-galaxy [core 2.20.5]" and go-galaxy answers
// a pseudo-version followed by build metadata: both belong in the report as
// provenance and neither belongs in a chart subtitle at full length.
func shortVersion(line string) string {
	if _, rest, found := strings.Cut(line, "[core "); found {
		if version, _, closed := strings.Cut(rest, "]"); closed {
			return version
		}
	}

	first, _, _ := strings.Cut(strings.TrimSpace(line), " ")

	return first
}

// collectionCount labels a row, in the singular where that is what it is.
func collectionCount(size int) string {
	if size == 1 {
		return "1 collection"
	}

	return fmt.Sprintf("%d collections", size)
}

// trimFloat prints a number without a trailing zero fraction, so 25 stays 25.
func trimFloat(value float64) string {
	text := fmt.Sprintf("%.2f", value)
	if strings.Contains(text, ".") {
		text = strings.TrimRight(text, "0")
		text = strings.TrimSuffix(text, ".")
	}

	return text
}
