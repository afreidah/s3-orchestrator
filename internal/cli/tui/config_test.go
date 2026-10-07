// -------------------------------------------------------------------------------
// TUI - Config View Tests
//
// Author: Alex Freidah
//
// Covers the config pane: loading on entry, the body's states, the
// pending-restart line, and the keys that reload and leave.
// -------------------------------------------------------------------------------

package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminapi"

	tea "github.com/charmbracelet/bubbletea"
)

// TestConfig_EntryLoadsAndRenders asserts entering the pane fetches the
// configuration and renders its YAML under the log level and pending fields.
func TestConfig_EntryLoadsAndRenders(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{config: &adminapi.ConfigResponse{
		Config:         "server:\n  listen_addr: :9000\n",
		LogLevel:       "debug",
		PendingRestart: []string{"database"},
	}})
	m.width, m.height = 120, 30

	_, cmd := m.selectSection(sectionConfig)
	if !m.config.loading {
		t.Error("entering with nothing loaded did not set loading")
	}
	m.Update(cmd())

	if got := m.configHeaderView(); !strings.Contains(got, "log level: debug") {
		t.Errorf("header = %q", got)
	}
	body := bodyText(m.configBody())
	if !strings.Contains(body, "pending restart: database") || !strings.Contains(body, "server:") {
		t.Errorf("body = %q", body)
	}
}

// TestConfigBody_States asserts the error, loading and not-yet-loaded bodies.
func TestConfigBody_States(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})

	m.config = configView{err: errors.New("boom")}
	if got := bodyText(m.configBody()); !strings.Contains(got, "boom") {
		t.Errorf("error body = %q", got)
	}
	m.config = configView{loading: true}
	if got := bodyText(m.configBody()); !strings.Contains(got, "loading") {
		t.Errorf("loading body = %q", got)
	}
	m.config = configView{}
	if got := bodyText(m.configBody()); !strings.Contains(got, "no configuration loaded") {
		t.Errorf("empty body = %q", got)
	}
}

// TestConfigKeys_ReloadAndBack asserts r refetches and esc hands focus back.
func TestConfigKeys_ReloadAndBack(t *testing.T) {
	t.Parallel()
	m := initialModel(&fakeLister{})
	m.section = sectionConfig

	_, cmd := m.handleConfigKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if !m.config.loading || cmd == nil {
		t.Error("r did not start a reload")
	}
	if _, ok := cmd().(configLoadedMsg); !ok {
		t.Errorf("reload result = %#v, want configLoadedMsg", cmd())
	}

	m.handleConfigKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !m.navFocus {
		t.Error("esc did not return focus to the nav")
	}
}

// TestLoadConfig_Error asserts a failed fetch becomes configErrMsg.
func TestLoadConfig_Error(t *testing.T) {
	t.Parallel()
	cmd := initialModel(errLister{}).loadConfig()
	if _, ok := cmd().(configErrMsg); !ok {
		t.Errorf("cmd result = %#v, want configErrMsg", cmd())
	}
}
