// -------------------------------------------------------------------------------
// TUI - Browser View Shaping
//
// Author: Alex Freidah
//
// Filtering and sorting for the object browser. The full loaded listing lives in
// the model's entries; this file derives the visible slice the table renders by
// applying the active substring filter and sort order. Navigation indexes into
// visible, so the cursor always maps to the row the operator sees.
// -------------------------------------------------------------------------------

package tui

import (
	"sort"
	"strconv"
	"strings"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// sortField selects how the listing is ordered. Directories are always grouped
// ahead of objects; the field orders rows within each group.
type sortField int

const (
	sortName sortField = iota // by name ascending
	sortSize                  // objects largest-first
)

// String renders the sort field for the status line.
func (s sortField) String() string {
	if s == sortSize {
		return "size"
	}
	return "name"
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// refreshVisible rebuilds visible from entries under the active filter and sort,
// then syncs the table rows. Call it whenever entries, the filter, or the sort
// order changes.
func (m *model) refreshVisible() {
	needle := strings.ToLower(m.filter.Value())
	m.visible = make([]entry, 0, len(m.entries))
	for _, e := range m.entries {
		if needle == "" || strings.Contains(strings.ToLower(e.name), needle) {
			m.visible = append(m.visible, e)
		}
	}
	sortEntries(m.visible, m.sort)
	m.table.SetRows(rowsFromEntries(m.visible))
}

// sortEntries orders rows in place: directories first, then the sort field
// within each group (name ascending, or object size descending).
func sortEntries(entries []entry, field sortField) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.isDir != b.isDir {
			return a.isDir
		}
		if field == sortSize && !a.isDir && a.size != b.size {
			return a.size > b.size
		}
		return a.name < b.name
	})
}

// statusLine describes the current filter and sort state for the footer. It is
// always present so the footer keeps a fixed height.
func (m *model) statusLine() string {
	parts := []string{"sort: " + m.sort.String()}
	if filter := m.filter.label(); filter != "" {
		parts = append(parts, "filter: "+filter, matchCount(len(m.visible), len(m.entries)))
	}
	if m.next != "" {
		parts = append(parts, "(more below)")
	}
	return strings.Join(parts, "   ")
}

// matchCount renders the filtered-vs-total row count shown while filtering.
func matchCount(shown, total int) string {
	return "(" + strconv.Itoa(shown) + " of " + strconv.Itoa(total) + " shown)"
}
