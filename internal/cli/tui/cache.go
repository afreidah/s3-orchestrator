// -------------------------------------------------------------------------------
// TUI - Cache View
//
// Author: Alex Freidah
//
// Read-only pane over the object data cache: how full it is and how well it is
// working. A fixed summary rather than a table, since the cache reports one set
// of numbers. Object caching is optional, so the endpoint answers 503 when it
// is off; that renders as a configuration notice, not an error. Reached with
// "c"; "esc" returns focus to the nav, "r" reloads.
// -------------------------------------------------------------------------------

package tui

import (
	"context"
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// cacheView holds the state of the object cache pane.
type cacheView struct {
	snap        *adminapi.CacheStatsResponse // last snapshot, nil until the first load
	loading     bool                         // a fetch is in flight
	unavailable string                       // set when object caching is disabled
	err         error                        // last fetch error, if any
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// cacheLoadedMsg carries a successfully loaded cache snapshot.
type cacheLoadedMsg struct{ resp *adminapi.CacheStatsResponse }

// cacheErrMsg carries a failed cache fetch.
type cacheErrMsg struct{ err error }

// loadCache returns a command that fetches the cache snapshot off the main loop.
func (m *model) loadCache() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		resp, err := client.GetCacheStats(context.Background())
		if err != nil {
			return cacheErrMsg{err}
		}
		return cacheLoadedMsg{resp}
	}
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// applyCache folds a loaded snapshot into the pane state.
func (m *model) applyCache(resp *adminapi.CacheStatsResponse) {
	m.cache.snap = resp
	m.cache.loading = false
	m.cache.unavailable = ""
	m.cache.err = nil
}

// applyCacheErr records a failed fetch, separating a deployment with caching
// switched off from a real failure.
func (m *model) applyCacheErr(err error) {
	m.cache.loading = false
	m.cache.unavailable = adminclient.UnavailableReason(err)
	m.cache.err = nil
	if m.cache.unavailable == "" {
		m.cache.err = err
	}
}

// handleCacheKey applies cache-pane keys (back, reload); the pane is a fixed
// summary, so there is nothing to scroll.
func (m *model) handleCacheKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc", "left", "h":
		m.navFocus = true
		m.navCursor = int(m.section)
		return m, nil
	case "r":
		m.cache.loading = m.cache.snap == nil
		cmd := m.loadCache()
		return m, cmd
	}
	return m, nil
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// cachePaneView composes the pane's full-screen layout.
func (m *model) cachePaneView() string {
	return m.frame(m.cacheHeaderView(), m.cacheFooterView(), m.cacheBodyView())
}

// cacheHeaderView renders the title bar.
func (m *model) cacheHeaderView() string {
	return m.contentTitleStyle().Width(m.contentWidth()).Render("object cache")
}

// cacheFooterView renders the cache key hints. Flushing lives on the Ops menu,
// which owns instance-wide actions.
func (m *model) cacheFooterView() string {
	return m.footer("r reload - tab nav - q quit")
}

// cacheBodyView renders the current content: an error, a disabled notice, the
// loading indicator, or the summary.
func (m *model) cacheBodyView() string {
	switch {
	case m.cache.err != nil:
		return errStyle.Render("error: " + m.cache.err.Error())
	case m.cache.unavailable != "":
		return pathStyle.Render("(" + m.cache.unavailable + ")")
	case m.cache.loading:
		return m.spinner.View() + " loading..."
	case m.cache.snap == nil:
		return pathStyle.Render("(no cache data)")
	default:
		return m.cacheStats()
	}
}

// cacheStats renders the snapshot as an aligned label/value block: capacity
// first, then effectiveness.
func (m *model) cacheStats() string {
	s := m.cache.snap
	const labelW = 14
	line := func(label, value string) string {
		return fmt.Sprintf("%-*s %s", labelW, label, value)
	}

	used := line("entries", fmt.Sprintf("%d", s.Entries))
	size := line("size", humanize.Bytes(s.SizeBytes))
	if s.MaxBytes > 0 {
		pct := usagePercent(s.SizeBytes, s.MaxBytes)
		size = line("size", fmt.Sprintf("%s / %s (%s)",
			humanize.Bytes(s.SizeBytes), humanize.Bytes(s.MaxBytes), usageStyle(pct).Render(fmt.Sprintf("%d%%", pct))))
	}

	lookups := s.Hits + s.Misses
	rate := line("hit rate", pathStyle.Render("no lookups yet"))
	if lookups > 0 {
		pct := int(s.Hits * 100 / lookups)
		rate = line("hit rate", hitRateStyle(pct).Render(fmt.Sprintf("%d%%", pct)))
	}
	served := line("lookups", fmt.Sprintf("%d hits / %d misses", s.Hits, s.Misses))

	return used + "\n" + size + "\n" + rate + "\n" + served
}

// hitRateStyle colours a hit rate: a cache serving most reads is doing its job,
// one serving almost none is spending memory for nothing. Inverted relative to
// usageStyle, where a high number is the warning.
func hitRateStyle(pct int) lipgloss.Style {
	switch {
	case pct >= 60:
		return statusOKStyle
	case pct >= 25:
		return logLevelWarn
	default:
		return statusErrStyle
	}
}
