// -------------------------------------------------------------------------------
// TUI - Browser View Tests
//
// Author: Alex Freidah
//
// Deterministic tests of size formatting, sort ordering, and the filter path:
// refreshVisible shaping, the filter-input key handling, and the browse-mode
// keys that toggle filtering and sorting.
// -------------------------------------------------------------------------------

package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestSortEntries(t *testing.T) {
	t.Parallel()
	// directories always sort ahead of objects, by name
	byName := []entry{{name: "b", size: 10}, {name: "a", isDir: true}, {name: "c", size: 30}, {name: "d", size: 20}}
	sortEntries(byName, sortName)
	if got := names(byName); got != "a,b,c,d" {
		t.Errorf("sortName = %q, want a,b,c,d", got)
	}

	// size orders objects largest-first, dirs still ahead
	bySize := []entry{{name: "b", size: 10}, {name: "a", isDir: true}, {name: "c", size: 30}, {name: "d", size: 20}}
	sortEntries(bySize, sortSize)
	if got := names(bySize); got != "a,c,d,b" {
		t.Errorf("sortSize = %q, want a,c,d,b", got)
	}
}

func TestRefreshVisible_Filter(t *testing.T) {
	t.Parallel()
	m := modelWith([]entry{{name: "apple"}, {name: "banana"}, {name: "cherry/", isDir: true}}, "", &fakeLister{})

	// case-insensitive substring narrows the visible set
	m.filter.SetValue("AN")
	m.refreshVisible()
	if len(m.visible) != 1 || m.visible[0].name != "banana" {
		t.Fatalf("filtered visible = %v, want [banana]", names(m.visible))
	}

	// clearing the filter restores every row
	m.clearFilter()
	m.refreshVisible()
	if len(m.visible) != 3 {
		t.Errorf("cleared visible = %d, want 3", len(m.visible))
	}
}

func TestHandleFilterKey(t *testing.T) {
	t.Parallel()
	m := modelWith([]entry{{name: "apple"}, {name: "banana"}}, "", &fakeLister{})
	m.filtering = true
	m.filter.Focus()

	// typing narrows the listing live
	m.handleFilterKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	if m.filter.Value() != "b" || len(m.visible) != 1 || m.visible[0].name != "banana" {
		t.Fatalf("after typing 'b': value=%q visible=%v", m.filter.Value(), names(m.visible))
	}

	// enter keeps the filter applied but returns focus to the table
	m.handleFilterKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.filtering || m.filter.Value() != "b" {
		t.Errorf("after enter: filtering=%v value=%q", m.filtering, m.filter.Value())
	}

	// esc abandons the filter entirely
	m.filtering = true
	m.handleFilterKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.filtering || m.filter.Value() != "" || len(m.visible) != 2 {
		t.Errorf("after esc: filtering=%v value=%q visible=%d", m.filtering, m.filter.Value(), len(m.visible))
	}
}

func TestHandleKey_FilterSortToggles(t *testing.T) {
	t.Parallel()
	// "/" enters filter mode
	m := modelWith([]entry{{name: "a"}}, "", &fakeLister{})
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if !m.filtering {
		t.Error("'/' did not enter filter mode")
	}

	// "s" toggles the sort field back and forth
	m = modelWith([]entry{{name: "a"}}, "", &fakeLister{})
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if m.sort != sortSize {
		t.Errorf("after one 's': sort = %v, want size", m.sort)
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	if m.sort != sortName {
		t.Errorf("after two 's': sort = %v, want name", m.sort)
	}

	// esc clears an applied filter from browse mode
	m = modelWith([]entry{{name: "apple"}, {name: "banana"}}, "", &fakeLister{})
	m.filter.SetValue("b")
	m.refreshVisible()
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.filter.Value() != "" || len(m.visible) != 2 {
		t.Errorf("esc in browse: value=%q visible=%d", m.filter.Value(), len(m.visible))
	}
}

func TestStatusLine(t *testing.T) {
	t.Parallel()
	m := modelWith([]entry{{name: "apple"}, {name: "banana"}}, "", &fakeLister{})

	// default: just the sort field
	if got := m.statusLine(); got != "sort: name" {
		t.Errorf("default status = %q", got)
	}

	// the size sort renders its own label
	m.sort = sortSize
	if got := m.statusLine(); got != "sort: size" {
		t.Errorf("size sort status = %q", got)
	}
	m.sort = sortName

	// with a filter: sort + filter + match count
	m.filter.SetValue("b")
	m.refreshVisible()
	if got := m.statusLine(); got != "sort: name   filter: b   (1 of 2 shown)" {
		t.Errorf("filtered status = %q", got)
	}
}

// names joins entry names with commas for compact assertions.
func names(entries []entry) string {
	out := ""
	for i, e := range entries {
		if i > 0 {
			out += ","
		}
		out += e.name
	}
	return out
}
