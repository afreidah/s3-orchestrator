// -------------------------------------------------------------------------------
// CLI Output - Format Selection and JSON Rendering
//
// Author: Alex Freidah
//
// Shared output helpers for the CLI subcommands. Commands render human-readable
// text by default and switch to raw JSON when the operator passes --json, so
// scripts keep a stable machine-readable contract while interactive use gets a
// readable summary. The JSON path pretty-prints the server's raw bytes rather
// than re-encoding a decoded value, keeping output byte-stable for pipelines.
// -------------------------------------------------------------------------------

package output

import (
	"bytes"
	"encoding/json"
	"io"
)

// Format selects how a command renders its result.
type Format string

const (
	FormatText Format = "text"
	FormatJSON Format = "json"
)

// FromJSON maps the --json boolean flag onto a Format. Absent flag renders
// human-readable text; present flag renders raw JSON.
func FromJSON(jsonFlag bool) Format {
	if jsonFlag {
		return FormatJSON
	}
	return FormatText
}

// IsJSON reports whether the format is raw JSON.
func (f Format) IsJSON() bool {
	return f == FormatJSON
}

// PrettyJSON writes raw JSON bytes to w, indented for readability. Bytes that
// do not parse as JSON are written through unchanged so a non-JSON error body
// still reaches the operator. Operating on raw bytes (rather than a decoded
// value) keeps key order and shape identical to what the server emitted.
func PrettyJSON(w io.Writer, raw []byte) error {
	var indented bytes.Buffer
	if err := json.Indent(&indented, raw, "", "  "); err != nil {
		_, werr := w.Write(raw)
		if werr == nil {
			_, werr = io.WriteString(w, "\n")
		}
		return werr
	}
	indented.WriteByte('\n')
	_, err := w.Write(indented.Bytes())
	return err
}
