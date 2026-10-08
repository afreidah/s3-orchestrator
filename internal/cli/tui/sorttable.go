// -------------------------------------------------------------------------------
// TUI - Sortable Tables
//
// Author: Alex Freidah
//
// A table over a list of items that the operator can reorder by any sortable
// column. It owns the items, so the pane hands over what it loaded and reads
// back the item under the cursor, and never builds rows or tracks order
// itself. "s" steps the sort through the sortable columns and back to the
// order the server sent; "S" flips the direction. Columns sort on the
// underlying values, so 900 MiB orders below 1.2 GiB.
// -------------------------------------------------------------------------------

package tui

import (
	"cmp"
	"slices"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
)

// Markers appended to the title of the column the table is sorted by.
const (
	sortAscMarker  = " ^"
	sortDescMarker = " v"
)

// tableSort is the column a table is ordered by, named by its title, and the
// direction. An empty column means the order the server sent.
type tableSort struct {
	column string
	desc   bool
}

// sortTable is a table over items of T. received holds the items in the order
// they were loaded; items holds them in display order, and the table's cursor
// indexes items.
type sortTable[T any] struct {
	table    table.Model
	columns  []columnSpec
	sorts    map[string]func(a, b *T) int
	rows     func([]T) []table.Row
	key      func(*T) string
	received []T
	items    []T
	sort     tableSort
}

// newSortTable builds a table whose sortable columns have a comparator in sorts,
// keyed by title. key identifies an item so the selection survives a re-sort.
func newSortTable[T any](columns []columnSpec, sorts map[string]func(a, b *T) int,
	rows func([]T) []table.Row, key func(*T) string) sortTable[T] {
	return sortTable[T]{table: newTable(columns), columns: columns, sorts: sorts, rows: rows, key: key}
}

// by builds a comparator from a field of T.
func by[T any, K cmp.Ordered](field func(*T) K) func(a, b *T) int {
	return func(a, b *T) int { return cmp.Compare(field(a), field(b)) }
}

// rank orders a flag for sorting: false before true.
func rank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// loaded reports whether items have been handed to the table yet.
func (s *sortTable[T]) loaded() bool {
	return s.received != nil
}

// setItems replaces the items, keeping the cursor on the item it was on.
func (s *sortTable[T]) setItems(items []T) {
	prev := s.selectedKey()
	s.received = items
	s.reorder(prev)
}

// selected returns the item under the cursor, or false when there is none.
func (s *sortTable[T]) selected() (*T, bool) {
	c := s.table.Cursor()
	if c < 0 || c >= len(s.items) {
		return nil, false
	}
	return &s.items[c], true
}

// selectedKey is the key of the item under the cursor, or "" when there is
// none.
func (s *sortTable[T]) selectedKey() string {
	if item, ok := s.selected(); ok {
		return s.key(item)
	}
	return ""
}

// update applies the sort keys and hands every other key to the table.
func (s *sortTable[T]) update(key tea.KeyMsg) tea.Cmd {
	switch key.String() {
	case "s":
		s.sort = tableSort{column: s.nextSortColumn()}
		s.reorder(s.selectedKey())
		return nil
	case "S":
		if s.sort.column != "" {
			s.sort.desc = !s.sort.desc
			s.reorder(s.selectedKey())
		}
		return nil
	}
	var cmd tea.Cmd
	s.table, cmd = s.table.Update(key)
	return cmd
}

// nextSortColumn is the sortable column after the current one, in column
// order, or "" for the server's order after the last.
func (s *sortTable[T]) nextSortColumn() string {
	passed := s.sort.column == ""
	for _, c := range s.columns {
		if _, ok := s.sorts[c.title]; !ok {
			continue
		}
		if passed {
			return c.title
		}
		passed = c.title == s.sort.column
	}
	return ""
}

// reorder rebuilds the display order and the rows from the received items,
// then puts the cursor back on the item keyed prev. A sort is stable, so
// items that compare equal keep the server's order.
func (s *sortTable[T]) reorder(prev string) {
	s.items = slices.Clone(s.received)
	if less, ok := s.sorts[s.sort.column]; ok {
		slices.SortStableFunc(s.items, func(a, b T) int {
			if s.sort.desc {
				return less(&b, &a)
			}
			return less(&a, &b)
		})
	}
	s.table.SetRows(s.rows(s.items))
	keys := make([]string, len(s.items))
	for i := range s.items {
		keys[i] = s.key(&s.items[i])
	}
	reselect(&s.table, keys, prev)
}

// specs returns the columns with the sorted one's title marked, for the pane
// to fit to its width. The marked column widens by the marker, so a
// fixed-width title is not truncated to make room for it.
func (s *sortTable[T]) specs() []columnSpec {
	if s.sort.column == "" {
		return s.columns
	}
	marker := sortAscMarker
	if s.sort.desc {
		marker = sortDescMarker
	}
	out := slices.Clone(s.columns)
	for i := range out {
		if out[i].title != s.sort.column {
			continue
		}
		out[i].title += marker
		out[i].min += len(marker)
		if out[i].max > 0 {
			out[i].max += len(marker)
		}
	}
	return out
}

// pane shows the table in the rows the frame gives it.
func (s *sortTable[T]) pane(m *model) pane {
	return m.tablePane(&s.table, s.specs())
}
