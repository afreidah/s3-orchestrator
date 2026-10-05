// -------------------------------------------------------------------------------
// TUI - Layout
//
// Author: Alex Freidah
//
// How a section's content area is divided. Vertically, a section is a header,
// a stack of panes, and a footer: each pane declares the rows it needs and
// whether it grows, and the rows left after the fixed ones are shared among the
// growing ones. Horizontally, every table declares its columns with a minimum
// width, a maximum width, and a priority, and the same fitting pass sizes all
// of them: when the minimums do not fit, the lowest-priority column is dropped,
// and the width left over widens the columns that can grow.
// -------------------------------------------------------------------------------

package tui

import (
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
)

// tableCellPad is the horizontal padding the bubbles table adds to every cell
// (one column on each side); the rendered table is this much wider per column
// than the sum of the declared column widths.
const tableCellPad = 2

// tableMinHeight is the fewest rows a table pane renders in: its header, the
// rule beneath it, and one data row.
const tableMinHeight = 3

// -------------------------------------------------------------------------
// COLUMNS
// -------------------------------------------------------------------------

// columnSpec declares one table column. A column whose max equals its min has a
// fixed width; a max of 0 lets the column take all the width left over. When
// the table is too narrow for every column, the one with the lowest priority is
// dropped first, and among equal priorities the rightmost goes first.
type columnSpec struct {
	title    string
	min      int
	max      int
	priority int
}

// fitColumns sizes specs to a table width. A dropped column gets width 0,
// which the bubbles table skips in both the header and the rows, so rows keep
// every cell whichever columns are showing. Spare width goes to the growable
// columns from left to right, each up to its max.
func fitColumns(width int, specs []columnSpec) []table.Column {
	kept := make([]bool, len(specs))
	for i := range kept {
		kept[i] = true
	}
	need := keptWidth(specs, kept)
	for need > width {
		drop := lowestPriority(specs, kept)
		if drop < 0 {
			break
		}
		kept[drop] = false
		need = keptWidth(specs, kept)
	}

	spare := width - need
	cols := make([]table.Column, len(specs))
	for i, s := range specs {
		cols[i] = table.Column{Title: s.title}
		if !kept[i] {
			continue
		}
		w := s.min
		if spare < 0 {
			// Only one column is left and even it does not fit.
			w = max(width-tableCellPad, 1)
		} else if s.max != s.min {
			grow := spare
			if s.max > 0 {
				grow = min(grow, s.max-s.min)
			}
			w += grow
			spare -= grow
		}
		cols[i].Width = w
	}
	return cols
}

// keptWidth is the width the kept columns need at their minimums, padding
// included.
func keptWidth(specs []columnSpec, kept []bool) int {
	total := 0
	for i, s := range specs {
		if kept[i] {
			total += s.min + tableCellPad
		}
	}
	return total
}

// lowestPriority returns the index of the next column to drop, or -1 when only
// one column is left, since a table with no columns shows nothing at all.
func lowestPriority(specs []columnSpec, kept []bool) int {
	drop, count := -1, 0
	for i, s := range specs {
		if !kept[i] {
			continue
		}
		count++
		if drop < 0 || s.priority <= specs[drop].priority {
			drop = i
		}
	}
	if count <= 1 {
		return -1
	}
	return drop
}

// newTable builds a focused table styled to match the browser: a bold accent
// header and the shared selected-row highlight. Its columns start at their
// minimum widths, because the first load can arrive before the first render and
// the table cannot take rows while it has no columns. The pane showing it fits
// the columns to the real width.
func newTable(specs []columnSpec) table.Model {
	cols := make([]table.Column, len(specs))
	for i, s := range specs {
		cols[i] = table.Column{Title: s.title, Width: s.min}
	}
	t := table.New(table.WithFocused(true), table.WithColumns(cols))
	t.SetStyles(tableStyles())
	return t
}

// tableStyles is the table's default styling with the theme's accent on the
// header and the shared selected-row highlight.
func tableStyles() table.Styles {
	st := table.DefaultStyles()
	st.Header = st.Header.Bold(true).Foreground(activeTheme.accent)
	st.Selected = selectedStyle
	return st
}

// -------------------------------------------------------------------------
// PANES
// -------------------------------------------------------------------------

// pane is one block of a section's content area. height is the number of rows
// the pane needs. A pane that grows is also given a share of the rows the fixed
// panes leave over. render draws the pane into the rows it was given.
type pane struct {
	height int
	grows  bool
	render func(height int) string
}

// textPane is a fixed block that takes exactly the rows its text needs: a
// summary, a notice, or a loading or error line.
func textPane(s string) pane {
	return pane{height: lipgloss.Height(s), render: func(int) string { return s }}
}

// tablePane is a growing block over a table. The table is sized when it is
// drawn, because that is when its rows are known: they depend on how tall the
// header and the other panes render. The table scrolls by its own height, so
// the size is set on the table itself rather than on a copy.
func (m *model) tablePane(t *table.Model, specs []columnSpec) pane {
	return pane{height: tableMinHeight, grows: true, render: func(height int) string {
		width := m.contentWidth()
		t.SetColumns(fitColumns(width, specs))
		t.SetWidth(width)
		t.SetHeight(height)
		return t.View()
	}}
}

// viewportPane is a growing block over a scrolling viewport. As with tables,
// the viewport is sized when it is drawn. The offset is then clamped again, so
// a viewport that was scrolled to the bottom while it was shorter still ends at
// its last line.
func (m *model) viewportPane(vp *viewport.Model) pane {
	return pane{height: 1, grows: true, render: func(height int) string {
		vp.Width = m.contentWidth()
		vp.Height = height
		vp.SetYOffset(vp.YOffset)
		return vp.View()
	}}
}

// frame stacks a header, the body panes, and a footer into the full-screen
// layout every section shares. The body fills the rows between the header and
// the footer, so the footer stays at the bottom however little the body holds.
// The header and footer are wrapped to the content width before they are
// measured, so a long status line takes another row instead of widening the
// whole pane.
func (m *model) frame(header, footer string, body ...pane) string {
	width := m.contentWidth()
	fit := lipgloss.NewStyle().Width(width)
	header, footer = fit.Render(header), fit.Render(footer)
	rows := max(m.height-lipgloss.Height(header)-lipgloss.Height(footer), 1)
	body = fitFixedPanes(width, body)
	heights := paneHeights(rows, body)

	blocks := make([]string, len(body))
	for i, p := range body {
		blocks[i] = lipgloss.NewStyle().Width(width).Height(heights[i]).MaxHeight(heights[i]).Render(p.render(heights[i]))
	}
	rendered := lipgloss.NewStyle().Width(width).Height(rows).MaxHeight(rows).
		Render(lipgloss.JoinVertical(lipgloss.Left, blocks...))
	return lipgloss.JoinVertical(lipgloss.Left, header, rendered, footer)
}

// fitFixedPanes wraps each fixed pane to the content width and takes its
// height from the wrapped text, so a line too long for the terminal costs the
// pane a row rather than pushing its last line out of view.
func fitFixedPanes(width int, panes []pane) []pane {
	fit := lipgloss.NewStyle().Width(width)
	out := make([]pane, len(panes))
	for i, p := range panes {
		if p.grows {
			out[i] = p
			continue
		}
		out[i] = textPane(fit.Render(p.render(p.height)))
	}
	return out
}

// paneHeights gives every pane the rows it needs, then shares out what is left
// among the growing panes, with the first ones getting a row more when the
// share does not divide evenly. When the panes need more rows than there are,
// they keep what they asked for and the frame cuts the bottom off.
func paneHeights(rows int, panes []pane) []int {
	heights := make([]int, len(panes))
	spare, growing := rows, 0
	for i, p := range panes {
		heights[i] = p.height
		spare -= p.height
		if p.grows {
			growing++
		}
	}
	if spare <= 0 || growing == 0 {
		return heights
	}
	share, extra := spare/growing, spare%growing
	for i, p := range panes {
		if !p.grows {
			continue
		}
		heights[i] += share
		if extra > 0 {
			heights[i]++
			extra--
		}
	}
	return heights
}
