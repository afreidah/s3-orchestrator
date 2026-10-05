// -------------------------------------------------------------------------------
// TUI - Theme Tests
//
// Author: Alex Freidah
//
// Covers parsing a theme spec (presets, overrides, and each way a spec can be
// wrong), where the spec comes from, and that a theme reaches the styles the
// panes draw with.
// -------------------------------------------------------------------------------

package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestParseTheme_PresetsAndOverrides verifies a spec starts from the named
// preset, or the default when it names none, and applies its overrides on top.
func TestParseTheme_PresetsAndOverrides(t *testing.T) {
	t.Parallel()
	cases := []struct {
		spec   string
		accent lipgloss.Color
		ok     lipgloss.Color
	}{
		{spec: "", accent: themePresets["dark"].accent, ok: themePresets["dark"].ok},
		{spec: "light", accent: themePresets["light"].accent, ok: themePresets["light"].ok},
		{spec: "high-contrast", accent: themePresets["high-contrast"].accent, ok: themePresets["high-contrast"].ok},
		{spec: "accent:#ff9e64", accent: "#ff9e64", ok: themePresets["dark"].ok},
		{spec: " light , accent:#abc , ok:114 ", accent: "#abc", ok: "114"},
	}
	for _, c := range cases {
		got, err := parseTheme(c.spec)
		if err != nil {
			t.Errorf("parseTheme(%q): %v", c.spec, err)
			continue
		}
		if got.accent != c.accent || got.ok != c.ok {
			t.Errorf("parseTheme(%q) accent=%s ok=%s, want %s %s", c.spec, got.accent, got.ok, c.accent, c.ok)
		}
	}
}

// TestParseTheme_EverySlotCanBeOverridden verifies every slot name in a spec
// sets a colour, so no slot is listed but unreachable.
func TestParseTheme_EverySlotCanBeOverridden(t *testing.T) {
	t.Parallel()
	for slot, field := range themeSlots {
		got, err := parseTheme(slot + ":#123456")
		if err != nil {
			t.Errorf("%s: %v", slot, err)
			continue
		}
		if *field(&got) != "#123456" {
			t.Errorf("%s was not set by its override", slot)
		}
	}
}

// TestBuildPresets_SetsEverySlot verifies each shipped preset defines every
// slot, and that a preset spec missing a slot or holding a bad colour panics
// rather than producing a partial theme.
func TestBuildPresets_SetsEverySlot(t *testing.T) {
	t.Parallel()
	for name, preset := range themePresets {
		for slot, field := range themeSlots {
			if *field(&preset) == "" {
				t.Errorf("preset %s leaves %s unset", name, slot)
			}
		}
	}
	for name, spec := range map[string]string{"missing slot": "accent:#ffffff", "bad colour": "accent:blue"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: buildPresets did not panic", name)
				}
			}()
			buildPresets(map[string]string{"broken": spec})
		}()
	}
}

// TestParseTheme_Rejects verifies each kind of bad spec fails with an error
// naming what was wrong.
func TestParseTheme_Rejects(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"solarized":            "unknown theme",
		"accent:#ff9e64,light": "must come before",
		"glow:#ffffff":         "unknown theme colour",
		"accent:blue":          "not a hex colour",
		"accent:#12345":        "not a hex colour",
		"accent:256":           "not a hex colour",
		"accent:-1":            "not a hex colour",
	}
	for spec, want := range cases {
		_, err := parseTheme(spec)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseTheme(%q) error = %v, want one containing %q", spec, err, want)
		}
	}
}

// TestParseArgs_ThemeSource verifies the theme comes from -theme ahead of
// $S3O_TUI_THEME, and that a bad spec is reported even when the target could
// not be resolved either.
func TestParseArgs_ThemeSource(t *testing.T) {
	t.Setenv(EnvTheme, "light")
	creds := []string{"-addr", "host:9000", "-access-key", "AK", "-secret-key", "SK"}

	_, th, err := parseArgs(creds)
	if err != nil || th.accent != themePresets["light"].accent {
		t.Errorf("from the environment: accent=%s err=%v, want the light preset", th.accent, err)
	}
	_, th, err = parseArgs(append([]string{"-theme", "high-contrast"}, creds...))
	if err != nil || th.accent != themePresets["high-contrast"].accent {
		t.Errorf("from the flag: accent=%s err=%v, want the flag to win", th.accent, err)
	}
	if _, _, err := parseArgs([]string{"-theme", "solarized"}); err == nil || !strings.Contains(err.Error(), "unknown theme") {
		t.Errorf("bad theme without a target: err=%v, want the theme error", err)
	}
}

// TestUseTheme_ReachesTheStyles verifies a theme's colours end up in the
// styles the panes and the table draw with. It changes the package's styles,
// so it does not run in parallel, and puts the default back afterwards.
func TestUseTheme_ReachesTheStyles(t *testing.T) {
	t.Cleanup(func() {
		def := themePresets[defaultThemeName]
		useTheme(&def)
	})
	th, err := parseTheme("light,accent:#010203")
	if err != nil {
		t.Fatal(err)
	}
	useTheme(&th)

	checks := map[string][2]any{
		"title background": {titleStyle.GetBackground(), th.titleBG},
		"active nav":       {navActiveStyle.GetForeground(), lipgloss.Color("#010203")},
		"status ok":        {statusOKStyle.GetForeground(), th.ok},
		"confirm":          {confirmStyle.GetBackground(), th.warn},
		"sidebar border":   {sidebarStyle.GetBorderRightForeground(), th.border},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %v, want %v", name, c[0], c[1])
		}
	}
	st := tableStyles()
	if st.Header.GetForeground() != lipgloss.Color("#010203") {
		t.Errorf("table header = %v, want the accent", st.Header.GetForeground())
	}
	if st.Selected.GetBackground() != th.selectedBG {
		t.Errorf("selected row = %v, want %v", st.Selected.GetBackground(), th.selectedBG)
	}
}
