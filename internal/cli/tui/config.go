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
	"context"
	"fmt"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// configView holds the state of the config pane.
type configView struct {
	resp    *adminapi.ConfigResponse
	vp      viewport.Model
	loading bool
	err     error
}

// -------------------------------------------------------------------------
// MESSAGES AND COMMANDS
// -------------------------------------------------------------------------

// configLoadedMsg carries a successfully loaded configuration.
type configLoadedMsg struct{ resp *adminapi.ConfigResponse }

// configErrMsg carries a failed configuration fetch.
type configErrMsg struct{ err error }

// loadConfig returns a command that fetches the running configuration off the
// main loop.
func (m *model) loadConfig() tea.Cmd {
	client := m.client
	return func() tea.Msg {
		resp, err := client.GetConfig(context.Background())
		if err != nil {
			return configErrMsg{err}
		}
		return configLoadedMsg{resp}
	}
}

// -------------------------------------------------------------------------
// TRANSITIONS
// -------------------------------------------------------------------------

// applyConfig folds a loaded configuration into the pane.
func (m *model) applyConfig(resp *adminapi.ConfigResponse) {
	m.config.resp = resp
	m.config.vp.SetContent(resp.Config)
	m.config.loading = false
	m.config.err = nil
}

// handleConfigKey applies pane keys (back, reload) and delegates scrolling to
// the viewport.
func (m *model) handleConfigKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "esc", "left", "h":
		return m.navBack()
	case "r":
		m.config.loading = true
		cmd := m.loadConfig()
		return m, cmd
	}

	var cmd tea.Cmd
	m.config.vp, cmd = m.config.vp.Update(key)
	return m, cmd
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

// configPaneView composes the pane's full-screen layout.
func (m *model) configPaneView() string {
	return m.frame(m.configHeaderView(), m.hintFooter(), m.configBody()...)
}

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
