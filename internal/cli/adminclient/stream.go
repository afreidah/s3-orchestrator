// -------------------------------------------------------------------------------
// Admin API Client - Progress Event Streams
//
// Author: Alex Freidah
//
// Long-running admin operations report progress as NDJSON: one
// adminstream.Event per line, terminated by a result event. EventStream is the
// iterator over that, so a caller can render events as they arrive without
// knowing whether they came off the wire or were synthesized locally.
// -------------------------------------------------------------------------------

package adminclient

import (
	"encoding/json"
	"io"

	"github.com/afreidah/s3-orchestrator/internal/transport/admin/adminstream"
)

// EventStream yields an operation's events in order, returning io.EOF once the
// operation is exhausted. Both the server's live NDJSON stream and a locally
// synthesized single result satisfy it, so callers consume every operation the
// same way.
type EventStream interface {
	Next() (adminstream.Event, error)
	Close() error
}

// decoderStream adapts a live NDJSON body to EventStream.
type decoderStream struct {
	dec  *json.Decoder
	body io.Closer
}

// newDecoderStream reads events off an open response body.
func newDecoderStream(body io.ReadCloser) EventStream {
	return &decoderStream{dec: json.NewDecoder(body), body: body}
}

func (s *decoderStream) Next() (adminstream.Event, error) {
	var e adminstream.Event
	err := s.dec.Decode(&e)
	return e, err
}

func (s *decoderStream) Close() error { return s.body.Close() }

// SliceStream replays a fixed set of events. Callers use it to present a
// one-shot action's single decoded result through the same path as a live
// stream, so rendering has one code path rather than two.
type SliceStream struct {
	events []adminstream.Event
	i      int
}

// NewSliceStream returns a stream over the supplied events.
func NewSliceStream(events ...adminstream.Event) *SliceStream {
	return &SliceStream{events: events}
}

func (s *SliceStream) Next() (adminstream.Event, error) {
	if s.i >= len(s.events) {
		return adminstream.Event{}, io.EOF
	}
	e := s.events[s.i]
	s.i++
	return e, nil
}

func (s *SliceStream) Close() error { return nil }
