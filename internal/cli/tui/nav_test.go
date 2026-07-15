// -------------------------------------------------------------------------------
// TUI - Left Navigation Tests
//
// Author: Alex Freidah
//
// Deterministic tests for the sidebar model: cursor bounds, section selection
// and its data-load side effect, focus toggling, the sidebar marker rendering,
// and the content-width / first-column layout math.
// -------------------------------------------------------------------------------

package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func TestContentWidth_FloorAndSubtraction(t *testing.T) {
	t.Parallel()
	// wide window: full width minus the sidebar and its divider.
	if got := (&model{width: 100}).contentWidth(); got != 100-sidebarWidth-1 {
		t.Errorf("wide contentWidth = %d", got)
	}
	// narrow window clamps to the 24-column floor.
	if got := (&model{width: 10}).contentWidth(); got != 24 {
		t.Errorf("narrow contentWidth = %d, want 24 floor", got)
	}
}

func TestFitFirstColumn(t *testing.T) {
	t.Parallel()
	const fixed, cols, maxWidth = 80, 9, 24
	// wide terminal: capped so short names don't sprawl.
	if got := fitFirstColumn(400, fixed, cols, maxWidth); got != maxWidth {
		t.Errorf("wide: got %d, want cap %d", got, maxWidth)
	}
	// narrow terminal: floored at 8, never collapses.
	if got := fitFirstColumn(40, fixed, cols, maxWidth); got != 8 {
		t.Errorf("narrow: got %d, want floor 8", got)
	}
	// middle: budget falls between the floor and the cap, so it fills the
	// leftover after the fixed columns and cell padding.
	want := 110 - fixed - cols*tableCellPad // = 12
	if got := fitFirstColumn(110, fixed, cols, maxWidth); got != want {
		t.Errorf("mid: got %d, want %d", got, want)
	}
}

func TestHandleNavKey_CursorBoundsAndOpen(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.navFocus = true

	// up at the top row is a no-op (stays at 0).
	m.handleNavKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.navCursor != 0 {
		t.Errorf("cursor after up-at-top = %d, want 0", m.navCursor)
	}
	// down advances to Backends and stops at the last selectable row.
	m.handleNavKey(tea.KeyMsg{Type: tea.KeyDown})
	m.handleNavKey(tea.KeyMsg{Type: tea.KeyDown}) // clamped: Logs is disabled
	if m.navCursor != selectableSections-1 {
		t.Errorf("cursor after two downs = %d, want %d", m.navCursor, selectableSections-1)
	}
	// enter on Backends selects the section, drops focus, and loads status.
	_, cmd := m.handleNavKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.section != sectionBackends || m.navFocus {
		t.Errorf("after enter: section=%v navFocus=%v", m.section, m.navFocus)
	}
	if cmd == nil {
		t.Fatal("selecting backends should return a load command")
	}
	if _, ok := cmd().(statusLoadedMsg); !ok {
		t.Errorf("load command result = %#v, want statusLoadedMsg", cmd())
	}
}

func TestHandleNavKey_EscDropsFocus(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.navFocus = true
	m.handleNavKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.navFocus {
		t.Error("esc should drop nav focus")
	}
}

func TestSelectSection_FilesHasNoLoad(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	_, cmd := m.selectSection(sectionFiles)
	if m.section != sectionFiles || m.navFocus {
		t.Errorf("section=%v navFocus=%v", m.section, m.navFocus)
	}
	if cmd != nil {
		t.Error("selecting files should not trigger a load")
	}
}

func TestSidebarView_MarkersAndStates(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.width, m.height = 80, 20

	// Idle on Files: the active marker leads Files, Logs shows the placeholder.
	out := m.sidebarView()
	if !strings.Contains(out, "> Files") {
		t.Errorf("Files should carry the active marker:\n%s", out)
	}
	if !strings.Contains(out, "soon") {
		t.Errorf("disabled Logs should render its placeholder:\n%s", out)
	}

	// While focused, the marker tracks the cursor, not the active section.
	m.navFocus = true
	m.navCursor = int(sectionBackends)
	out = m.sidebarView()
	if !strings.Contains(out, "> Backends") {
		t.Errorf("focused cursor should mark Backends:\n%s", out)
	}
}

func TestHandleKey_TabTogglesNavAndLetterJumps(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})

	// tab focuses the nav and seeds the cursor from the active section.
	m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	if !m.navFocus || m.navCursor != int(m.section) {
		t.Errorf("after tab: navFocus=%v cursor=%d", m.navFocus, m.navCursor)
	}
	// tab again toggles focus back off.
	m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	if m.navFocus {
		t.Error("second tab should drop focus")
	}
	// "b" jumps straight to Backends and loads it.
	_, cmd := m.handleKey(key("b"))
	if m.section != sectionBackends || cmd == nil {
		t.Errorf("b jump: section=%v cmd=%v", m.section, cmd)
	}
	// "f" jumps back to Files.
	m.handleKey(key("f"))
	if m.section != sectionFiles {
		t.Errorf("f jump: section=%v", m.section)
	}
}
