// -------------------------------------------------------------------------------
// TUI - Pane Loads
//
// Author: Alex Freidah
//
// How a pane fetches what it shows. fetchCmd runs the request off the main
// loop and delivers one loadDone message that knows how to fold its own
// outcome into the model, so the update loop never names a pane's response
// type. loadState is what every pane remembers about its last fetch.
// -------------------------------------------------------------------------------

package tui

import (
	"context"

	"github.com/afreidah/s3-orchestrator/internal/cli/adminclient"

	tea "github.com/charmbracelet/bubbletea"
)

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

// failed records a failed fetch, separating an endpoint the deployment did not
// wire from a real failure.
func (s *loadState) failed(err error) {
	s.loading, s.err = false, nil
	if s.unavailable = adminclient.UnavailableReason(err); s.unavailable == "" {
		s.err = err
	}
}

// loadDone is a finished fetch: the poll target it answers, or noPoll, and the
// fold of its outcome into the model.
type loadDone struct {
	target pollTarget
	apply  func(*model)
}

// fetchCmd calls get off the main loop and delivers a loadDone that applies the
// response with ok or the error with fail. The returned function must not touch
// the model.
func fetchCmd[T any](target pollTarget, get func(context.Context) (T, error), ok func(*model, T), fail func(*model, error)) tea.Cmd {
	return func() tea.Msg {
		resp, err := get(context.Background())
		if err != nil {
			return loadDone{target, func(m *model) { fail(m, err) }}
		}
		return loadDone{target, func(m *model) { ok(m, resp) }}
	}
}
