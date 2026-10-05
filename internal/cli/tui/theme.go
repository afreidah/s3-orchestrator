// -------------------------------------------------------------------------------
// TUI - Themes
//
// Author: Alex Freidah
//
// The TUI's colours, as a theme of named slots that every style is built from.
// A theme is chosen by a spec string from the -theme flag or $S3O_TUI_THEME,
// in the form fzf uses for --color: an optional preset name, then overrides
// as slot:colour pairs, all comma-separated. "light", "dark,accent:#ff9e64"
// and "ok:114" are all valid specs. A colour is a hex value (#rgb or #rrggbb)
// or a 256-colour code from 0 to 255.
// -------------------------------------------------------------------------------

package tui

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// EnvTheme carries the theme spec when the -theme flag is not given.
const EnvTheme = "S3O_TUI_THEME"

// defaultThemeName is the preset used when the spec names none.
const defaultThemeName = "dark"

// theme is the set of colours the TUI draws with. Each slot names a role
// rather than a colour, so a preset for a light terminal can change every
// value without changing what any of them is for.
type theme struct {
	titleBG    lipgloss.Color
	titleFG    lipgloss.Color
	surface    lipgloss.Color
	accent     lipgloss.Color
	header     lipgloss.Color
	selectedBG lipgloss.Color
	selectedFG lipgloss.Color
	text       lipgloss.Color
	muted      lipgloss.Color
	border     lipgloss.Color
	ok         lipgloss.Color
	warn       lipgloss.Color
	err        lipgloss.Color
}

// themeSlots maps each slot's name in a spec to the field it sets. The
// title bar and the selected row each have a background and a foreground;
// surface is the muted title bar's background; accent marks the active nav
// entry, the focused divider and the table headers; header colours the logs
// column header and the inspector's tag label; muted is paths and notices.
var themeSlots = map[string]func(*theme) *lipgloss.Color{
	"title-bg":    func(t *theme) *lipgloss.Color { return &t.titleBG },
	"title-fg":    func(t *theme) *lipgloss.Color { return &t.titleFG },
	"surface":     func(t *theme) *lipgloss.Color { return &t.surface },
	"accent":      func(t *theme) *lipgloss.Color { return &t.accent },
	"header":      func(t *theme) *lipgloss.Color { return &t.header },
	"selected-bg": func(t *theme) *lipgloss.Color { return &t.selectedBG },
	"selected-fg": func(t *theme) *lipgloss.Color { return &t.selectedFG },
	"text":        func(t *theme) *lipgloss.Color { return &t.text },
	"muted":       func(t *theme) *lipgloss.Color { return &t.muted },
	"border":      func(t *theme) *lipgloss.Color { return &t.border },
	"ok":          func(t *theme) *lipgloss.Color { return &t.ok },
	"warn":        func(t *theme) *lipgloss.Color { return &t.warn },
	"error":       func(t *theme) *lipgloss.Color { return &t.err },
}

// presetSpecs defines each preset in the same slot:colour syntax a user's
// overrides use, and every preset sets every slot. dark is Tokyo Night, light
// is its Day variant for light terminals, and high-contrast uses saturated
// colours on a black background.
var presetSpecs = map[string]string{
	"dark": "title-bg:#7aa2f7,title-fg:#1a1b26,surface:#292e42,accent:#7dcfff,header:#bb9af7," +
		"selected-bg:#364a82,selected-fg:#c0caf5,text:#c0caf5,muted:#737aa2,border:#3b4261," +
		"ok:#9ece6a,warn:#e0af68,error:#f7768e",
	"light": "title-bg:#2e7de9,title-fg:#e1e2e7,surface:#c4c8da,accent:#007197,header:#9854f1," +
		"selected-bg:#b7c1e3,selected-fg:#3760bf,text:#3760bf,muted:#848cb5,border:#a8aecb," +
		"ok:#587539,warn:#8c6c3e,error:#f52a65",
	"high-contrast": "title-bg:#ffff00,title-fg:#000000,surface:#444444,accent:#00ffff,header:#ff00ff," +
		"selected-bg:#ffff00,selected-fg:#000000,text:#ffffff,muted:#c0c0c0,border:#ffffff," +
		"ok:#00ff00,warn:#ffaf00,error:#ff0000",
}

// themePresets are the named themes a spec can start from, built from
// presetSpecs.
var themePresets = buildPresets(presetSpecs)

// buildPresets parses each preset spec into a theme. The specs are fixed
// data, so a slot left unset or a bad colour is a programming error and
// panics when the package loads, rather than leaving a preset that draws in
// the terminal's default colour.
func buildPresets(specs map[string]string) map[string]theme {
	presets := make(map[string]theme, len(specs))
	for name, spec := range specs {
		var t theme
		for part := range strings.SplitSeq(spec, ",") {
			slot, colour, _ := strings.Cut(part, ":")
			if err := t.set(slot, colour); err != nil {
				panic(fmt.Sprintf("theme preset %s: %v", name, err))
			}
		}
		for slot, field := range themeSlots {
			if *field(&t) == "" {
				panic(fmt.Sprintf("theme preset %s does not set %s", name, slot))
			}
		}
		presets[name] = t
	}
	return presets
}

// hexColour matches a #rgb or #rrggbb colour.
var hexColour = regexp.MustCompile(`^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// parseTheme builds a theme from a spec. An empty spec is the default preset.
// A preset name may only come first, since an override placed before it
// would be silently discarded when the preset replaced every slot.
func parseTheme(spec string) (theme, error) {
	t := themePresets[defaultThemeName]
	for i, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, isOverride := strings.Cut(part, ":")
		if !isOverride {
			preset, ok := themePresets[name]
			switch {
			case !ok:
				return theme{}, fmt.Errorf("unknown theme %q (presets: %s)", name, strings.Join(slices.Sorted(maps.Keys(themePresets)), ", "))
			case i > 0:
				return theme{}, fmt.Errorf("theme preset %q must come before any overrides", name)
			}
			t = preset
			continue
		}
		if err := t.set(strings.TrimSpace(name), strings.TrimSpace(value)); err != nil {
			return theme{}, err
		}
	}
	return t, nil
}

// set overrides one slot with a colour, rejecting unknown slots and anything
// that is not a hex colour or a 256-colour code.
func (t *theme) set(slot, colour string) error {
	field, ok := themeSlots[slot]
	if !ok {
		return fmt.Errorf("unknown theme colour %q (colours: %s)", slot, strings.Join(slices.Sorted(maps.Keys(themeSlots)), ", "))
	}
	if !validColour(colour) {
		return fmt.Errorf("theme colour %s: %q is not a hex colour (#rgb or #rrggbb) or a 256-colour code (0-255)", slot, colour)
	}
	*field(t) = lipgloss.Color(colour)
	return nil
}

// validColour reports whether s is a hex colour or a 256-colour code.
func validColour(s string) bool {
	if hexColour.MatchString(s) {
		return true
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= 0 && n <= 255
}
