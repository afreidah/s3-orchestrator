// -------------------------------------------------------------------------------
// CLI Output - Human-Readable JSON Value Rendering
//
// Author: Alex Freidah
//
// Renders a decoded JSON response as an indented, YAML-like text block: one
// "key: value" line per object field, "-" bullets per array element, nested
// structures indented two spaces per level. Scalars print without JSON quoting
// or float exponent noise, so an operator scans a response far faster than the
// brace-delimited form. Input that does not parse as JSON is written through
// unchanged so an unexpected body still reaches the operator.
// -------------------------------------------------------------------------------

package output

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// RenderValue decodes raw JSON and writes it as an indented, YAML-like text
// block to w. Non-JSON input is written through unchanged.
func RenderValue(w io.Writer, raw []byte) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		if _, werr := w.Write(raw); werr != nil {
			return werr
		}
		_, werr := io.WriteString(w, "\n")
		return werr
	}
	return writeValue(w, v, 0)
}

// writeValue dispatches on the dynamic type of a decoded JSON value, writing
// it at the given indent level.
func writeValue(w io.Writer, v any, indent int) error {
	switch t := v.(type) {
	case map[string]any:
		return writeMap(w, t, indent)
	case []any:
		return writeSlice(w, t, indent)
	default:
		_, err := fmt.Fprintf(w, "%s%s\n", indentOf(indent), scalar(v))
		return err
	}
}

// writeMap writes object fields as aligned "key: value" lines, recursing into
// nested objects and arrays under a "key:" header. Keys render in sorted order
// so output is deterministic across runs.
func writeMap(w io.Writer, m map[string]any, indent int) error {
	prefix := indentOf(indent)
	for _, k := range sortedKeys(m) {
		switch val := m[k]; val.(type) {
		case map[string]any, []any:
			if _, err := fmt.Fprintf(w, "%s%s:\n", prefix, k); err != nil {
				return err
			}
			if err := writeValue(w, val, indent+1); err != nil {
				return err
			}
		default:
			if _, err := fmt.Fprintf(w, "%s%s: %s\n", prefix, k, scalar(val)); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeSlice writes array elements as "-" bullets, recursing into nested
// objects and arrays one level deeper.
func writeSlice(w io.Writer, s []any, indent int) error {
	prefix := indentOf(indent)
	for _, item := range s {
		switch item.(type) {
		case map[string]any, []any:
			if _, err := fmt.Fprintf(w, "%s-\n", prefix); err != nil {
				return err
			}
			if err := writeValue(w, item, indent+1); err != nil {
				return err
			}
		default:
			if _, err := fmt.Fprintf(w, "%s- %s\n", prefix, scalar(item)); err != nil {
				return err
			}
		}
	}
	return nil
}

// scalar formats a decoded JSON scalar for text output: booleans and strings
// verbatim, null as "null", and numbers without exponent notation or trailing
// zeros (json.Unmarshal decodes every number as float64).
func scalar(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case string:
		return t
	default:
		return fmt.Sprintf("%v", t)
	}
}

// sortedKeys returns the map keys in sorted order for deterministic output.
func sortedKeys(m map[string]any) []string {
	return slices.Sorted(maps.Keys(m))
}

// indentOf returns the leading whitespace for the given nesting level.
func indentOf(level int) string {
	return strings.Repeat("  ", level)
}
