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
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"
	"github.com/afreidah/s3-orchestrator/internal/util/humanize"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// cacheView holds the state of the object cache pane.
type cacheView struct {
	loadState
	snap *adminapi.CacheStatsResponse // last snapshot, nil until the first load
}

// loadState is what a pane knows about its last fetch: whether one is in
// flight, the reason the deployment does not serve the pane, or the error the
// fetch failed with.
type loadState struct {
	loading     bool
	unavailable string
	err         error
}

// loaded records a fetch that succeeded.
func (s *loadState) loaded() {
	s.loading, s.unavailable, s.err = false, "", nil
}

// fail records a failed fetch as an error, for a pane every deployment serves.
func (s *loadState) fail(err error) {
	s.loading, s.err = false, err
}

// failed records a failed fetch, separating an endpoint the deployment did not
// wire from a real failure.
func (s *loadState) failed(err error) {
	s.loading, s.err = false, nil
	if s.unavailable = adminclient.UnavailableReason(err); s.unavailable == "" {
		s.err = err
	}
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// loadCache returns a command that fetches the cache snapshot off the main loop.
func (m *model) loadCache() tea.Cmd {
	return fetchCmd(m.client.GetCacheStats)
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// applyCache folds a loaded snapshot into the pane state.
func (m *model) applyCache(resp *adminapi.CacheStatsResponse) {
	m.cache.snap = resp
	m.cache.loaded()
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// cacheHeaderView renders the title bar.
func (m *model) cacheHeaderView() string {
	return m.contentTitleStyle().Width(m.contentWidth()).Render("object cache")
}

// cacheBody renders the current content: an error, a disabled notice, the
// loading indicator, or the summary.
func (m *model) cacheBody() []pane {
	return m.paneBody(m.cache.err, m.cache.unavailable, m.cache.loading, func() []pane {
		if m.cache.snap == nil {
			return []pane{textPane(pathStyle.Render("(no cache data)"))}
		}
		return []pane{textPane(m.cacheStats())}
	})
}

// cacheStats renders the snapshot as an aligned label/value block: capacity
// first, then effectiveness.
func (m *model) cacheStats() string {
	s := m.cache.snap
	size := humanize.Bytes(s.SizeBytes)
	if s.MaxBytes > 0 {
		size = fmt.Sprintf("%s / %s (%s)", humanize.Bytes(s.SizeBytes), humanize.Bytes(s.MaxBytes),
			percentText(humanize.Percent(s.SizeBytes, s.MaxBytes), 0))
	}

	rate := pathStyle.Render("no lookups yet")
	if lookups := s.Hits + s.Misses; lookups > 0 {
		pct := humanize.Percent(s.Hits, lookups)
		rate = hitRateStyle(pct).Render(fmt.Sprintf("%d%%", pct))
	}

	return fieldBlock(summaryLabelWidth,
		field{"entries", fmt.Sprintf("%d", s.Entries)},
		field{"size", size},
		field{"hit rate", rate},
		field{"lookups", fmt.Sprintf("%d hits / %d misses", s.Hits, s.Misses)},
	)
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
