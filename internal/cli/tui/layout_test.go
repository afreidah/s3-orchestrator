// -------------------------------------------------------------------------------
// TUI - Layout Tests
//
// Author: Alex Freidah
//
// Covers how table columns are fitted to a width (growing, capping, and the
// order columns are dropped in), how a body's rows are shared among its panes,
// and that a whole section renders to exactly the terminal's size, with the
// footer on the last line, at both a wide and a narrow width.
// -------------------------------------------------------------------------------

package tui

import (
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
)

// bodyText renders body panes at the rows each one asks for, for tests that
// check what a pane says rather than where it lands.
func bodyText(panes []pane) string {
	out := make([]string, len(panes))
	for i, p := range panes {
		out[i] = p.render(p.height)
	}
	return strings.Join(out, "\n")
}

// widths lists the width fitColumns gave each column, in order.
func widths(cols []table.Column) []int {
	out := make([]int, len(cols))
	for i, c := range cols {
		out[i] = c.Width
	}
	return out
}

// equalInts reports whether two int slices hold the same values.
func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// -------------------------------------------------------------------------
// COLUMNS
// -------------------------------------------------------------------------

// testColumns is a capped name, two fixed columns, and a column that takes the
// rest.
var testColumns = []columnSpec{
	{title: "NAME", min: 8, max: 20, priority: 4},
	{title: "A", min: 10, max: 10, priority: 1},
	{title: "B", min: 10, max: 10, priority: 2},
	{title: "REST", min: 8, max: 0, priority: 3},
}

// TestFitColumns_GrowsLeftToRightWithinCaps verifies spare width widens the
// capped column up to its cap and gives everything after that to the
// unbounded column, while fixed columns keep their width.
func TestFitColumns_GrowsLeftToRightWithinCaps(t *testing.T) {
	t.Parallel()
	// The minimums plus padding need 8+10+10+8 + 4*2 = 44, leaving 56 spare:
	// 12 take NAME to its cap of 20 and the other 44 go to REST.
	got := widths(fitColumns(100, testColumns))
	if want := []int{20, 10, 10, 52}; !equalInts(got, want) {
		t.Errorf("widths = %v, want %v", got, want)
	}
}

// TestFitColumns_DropsLowestPriorityFirst verifies columns go in priority
// order as the width shrinks, and a dropped column has width 0, which the
// table skips.
func TestFitColumns_DropsLowestPriorityFirst(t *testing.T) {
	t.Parallel()
	cases := []struct {
		width int
		want  []int
	}{
		{width: 44, want: []int{8, 10, 10, 8}},
		{width: 43, want: []int{19, 0, 10, 8}},
		{width: 32, want: []int{8, 0, 10, 8}},
		{width: 31, want: []int{19, 0, 0, 8}},
		{width: 19, want: []int{17, 0, 0, 0}},
	}
	for _, c := range cases {
		if got := widths(fitColumns(c.width, testColumns)); !equalInts(got, c.want) {
			t.Errorf("width %d: widths = %v, want %v", c.width, got, c.want)
		}
	}
}

// TestFitColumns_TiesDropTheRightmost verifies that among columns of equal
// priority the rightmost is dropped first.
func TestFitColumns_TiesDropTheRightmost(t *testing.T) {
	t.Parallel()
	specs := []columnSpec{
		{title: "KEEP", min: 10, max: 10, priority: 2},
		{title: "LEFT", min: 10, max: 10, priority: 1},
		{title: "RIGHT", min: 10, max: 10, priority: 1},
	}
	got := widths(fitColumns(24, specs))
	if want := []int{10, 10, 0}; !equalInts(got, want) {
		t.Errorf("widths = %v, want %v", got, want)
	}
}

// TestFitColumns_LastColumnShrinksBelowItsMinimum verifies the column with the
// highest priority is never dropped, and is narrowed instead when even it
// does not fit.
func TestFitColumns_LastColumnShrinksBelowItsMinimum(t *testing.T) {
	t.Parallel()
	if got := widths(fitColumns(6, testColumns)); !equalInts(got, []int{4, 0, 0, 0}) {
		t.Errorf("widths = %v, want the name narrowed to fit", got)
	}
	if got := widths(fitColumns(0, testColumns)); !equalInts(got, []int{1, 0, 0, 0}) {
		t.Errorf("widths = %v, want the name kept at one column", got)
	}
}

// -------------------------------------------------------------------------
// PANES
// -------------------------------------------------------------------------

// TestPaneHeights_SharesSpareAmongGrowingPanes verifies fixed panes keep their
// height and the spare rows are split among the growing ones, with the first
// taking the odd row.
func TestPaneHeights_SharesSpareAmongGrowingPanes(t *testing.T) {
	t.Parallel()
	panes := []pane{{height: 2}, {height: 3, grows: true}, {height: 1}, {height: 1, grows: true}}
	// 7 rows are needed and 20 are available, so 13 are spare: 7 and 6.
	if got := paneHeights(20, panes); !equalInts(got, []int{2, 10, 1, 7}) {
		t.Errorf("heights = %v, want [2 10 1 7]", got)
	}
}

// TestPaneHeights_NothingToShare verifies the panes keep the rows they asked
// for when none grows or when there are fewer rows than they need.
func TestPaneHeights_NothingToShare(t *testing.T) {
	t.Parallel()
	fixed := []pane{{height: 2}, {height: 3}}
	if got := paneHeights(20, fixed); !equalInts(got, []int{2, 3}) {
		t.Errorf("fixed heights = %v, want [2 3]", got)
	}
	tight := []pane{{height: 2}, {height: 3, grows: true}}
	if got := paneHeights(4, tight); !equalInts(got, []int{2, 3}) {
		t.Errorf("tight heights = %v, want [2 3]", got)
	}
}

// TestFrame_FillsTheTerminal verifies a section renders to exactly the
// terminal's height with the footer on the last line, whatever the header
// adds. The backends header gains a line while a drain is followed, and the
// table has to give that row up rather than push the footer off the screen.
func TestFrame_FillsTheTerminal(t *testing.T) {
	t.Parallel()
	m := backendsModel(t, &fakeLister{})
	m.height = 12
	m.beginDrainWatch("minio-a")

	lines := strings.Split(m.contentView(), "\n")
	if len(lines) != m.height {
		t.Fatalf("rendered %d lines, want %d", len(lines), m.height)
	}
	if last := lines[len(lines)-1]; !strings.Contains(last, "q quit") {
		t.Errorf("last line = %q, want the footer", last)
	}
	if view := m.contentView(); !strings.Contains(view, "minio-b") {
		t.Errorf("the table lost its last row:\n%s", view)
	}
}

// TestFrame_StacksPanesInOrder verifies the inspector's tag line sits above
// its table and both fit in the frame.
func TestFrame_StacksPanesInOrder(t *testing.T) {
	t.Parallel()
	m := modelWith(nil, "p/", &fakeLister{})
	m.width, m.height = 120, 10
	m.mode = modeInspect
	m.insp.table = newTable(inspectorColumns)
	m.applyLocations(&adminapi.ObjectLocationsResponse{Locations: []adminapi.ObjectLocation{{Backend: "minio-1"}}})
	m.applyTags(tagsLoadedMsg{tags: []adminapi.ObjectTag{{Key: "retain", Value: "30d"}}})

	view := m.contentView()
	tags, row := strings.Index(view, "retain=30d"), strings.Index(view, "minio-1")
	if tags < 0 || row < 0 || tags > row {
		t.Errorf("want the tag line above the copy row:\n%s", view)
	}
	if got := lipgloss.Height(view); got != m.height {
		t.Errorf("rendered %d lines, want %d", got, m.height)
	}
}

// TestBackendsPane_NarrowTerminal verifies a narrow terminal drops the
// backends table's low-priority columns rather than running off the edge.
func TestBackendsPane_NarrowTerminal(t *testing.T) {
	t.Parallel()
	m := backendsModel(t, &fakeLister{})
	m.width = 70

	view := m.contentView()
	for _, line := range strings.Split(view, "\n") {
		if w := lipgloss.Width(line); w > m.contentWidth() {
			t.Fatalf("line is %d wide, past the %d-column content area: %q", w, m.contentWidth(), line)
		}
	}
	for _, want := range []string{"BACKEND", "HEALTH", "USE%", "minio-a"} {
		if !strings.Contains(view, want) {
			t.Errorf("narrow view dropped %q:\n%s", want, view)
		}
	}
	for _, gone := range []string{"SAVED", "API"} {
		if strings.Contains(view, gone) {
			t.Errorf("narrow view kept low-priority column %q:\n%s", gone, view)
		}
	}
}

// TestTablePanes_FitANarrowTerminal renders every section that shows a table at
// a narrow width and checks no line runs past the content area.
func TestTablePanes_FitANarrowTerminal(t *testing.T) {
	t.Parallel()
	workers := func(t *testing.T) *model {
		t.Helper()
		m := initialModel(&fakeLister{})
		m.section = sectionWorkers
		m.applyWorkers(&adminapi.WorkersResponse{Workers: []adminapi.WorkerHealth{
			{Name: "scrubber", ConsecutiveFailures: 2, LastError: "backend unreachable after three attempts"},
		}})
		return m
	}
	dlq := func(t *testing.T) *model {
		t.Helper()
		m := cleanupModel(t)
		m.cleanup.tab = cleanupTabDLQ
		return m
	}
	files := func(t *testing.T) *model {
		t.Helper()
		m := modelWith([]entry{{name: "photos/", isDir: true}, {name: "a-rather-long-object-name.jpg", size: 2048}}, "", &fakeLister{})
		m.loading = false
		return m
	}
	builders := map[string]func(*testing.T) *model{
		"backends": func(t *testing.T) *model { return backendsModel(t, &fakeLister{}) },
		"buckets":  bucketsModel,
		"workers":  workers,
		"cleanup":  cleanupModel,
		"dlq":      dlq,
		"files":    files,
	}
	for name, build := range builders {
		m := build(t)
		m.width, m.height = 60, 15
		view := m.contentView()
		if got := lipgloss.Height(view); got != m.height {
			t.Errorf("%s: rendered %d lines, want %d", name, got, m.height)
		}
		for _, line := range strings.Split(view, "\n") {
			if w := lipgloss.Width(line); w > m.contentWidth() {
				t.Errorf("%s: line is %d wide, past the %d-column content area: %q", name, w, m.contentWidth(), line)
			}
		}
	}
}

// TestViewportPane_ClampsAfterGrowing verifies a viewport scrolled to the
// bottom before it had a height still ends at its last line once its pane
// sizes it.
func TestViewportPane_ClampsAfterGrowing(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 120, 10
	m.section = sectionOps
	m.ops = opsView{actions: opsActions()}
	for _, line := range []string{"one", "two", "three"} {
		m.appendOpsLine(line)
	}
	m.ops.showOut = true

	view := m.contentView()
	for _, want := range []string{"one", "two", "three"} {
		if !strings.Contains(view, want) {
			t.Errorf("output missing %q:\n%s", want, view)
		}
	}
}
