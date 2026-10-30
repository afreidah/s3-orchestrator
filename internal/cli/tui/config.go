// -------------------------------------------------------------------------------
// TUI - Config View
//
// Author: Alex Freidah
//
// Read-only pane over the running configuration: the YAML the server is using,
// with secrets redacted, under the log level in effect and any file changes
// that wait on a restart. Reached with "n"; "esc" returns focus to the nav, "r"
// reloads.
// -------------------------------------------------------------------------------

package tui

import (
	"fmt"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// configView holds the state of the config pane.
type configView struct {
	loadState
	resp *adminapi.ConfigResponse
	vp   viewport.Model
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// loadConfig returns a command that fetches the running configuration off the
// main loop.
func (m *model) loadConfig() tea.Cmd {
	return fetchCmd(m.client.GetConfig)
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// applyConfig folds a loaded configuration into the pane.
func (m *model) applyConfig(resp *adminapi.ConfigResponse) {
	m.config.resp = resp
	m.config.vp.SetContent(resp.Config)
	m.config.loaded()
}

// handleConfigKey scrolls the configuration.
func (m *model) handleConfigKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.config.vp, cmd = m.config.vp.Update(key)
	return m, cmd
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// configHeaderView renders the title bar with the log level in effect.
func (m *model) configHeaderView() string {
	title := "config"
	if m.config.resp != nil {
		title = fmt.Sprintf("config   log level: %s", m.config.resp.LogLevel)
	}
	return m.contentTitleStyle().Width(m.contentWidth()).Render(title)
}

// configBody renders the pending-restart fields above the scrolling YAML.
func (m *model) configBody() []pane {
	return m.paneBody(m.config.err, "", m.config.loading, func() []pane {
		if m.config.resp == nil {
			return []pane{textPane(pathStyle.Render("(no configuration loaded)"))}
		}
		var panes []pane
		if pending := m.config.resp.PendingRestart; len(pending) > 0 {
			panes = append(panes, textPane(logLevelWarn.Render("pending restart: "+strings.Join(pending, ", "))))
		}
		return append(panes, m.viewportPane(&m.config.vp))
	})
}
