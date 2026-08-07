// -------------------------------------------------------------------------------
// UI Templates - Embedded HTML and Static Assets
//
// Author: Alex Freidah
//
// Loads HTML templates and static files from embedded filesystem. Provides
// template helper functions for formatting bytes, percentages, and numbers.
// -------------------------------------------------------------------------------

package ui

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"strings"

	"github.com/afreidah/s3-orchestrator/internal/util/humanize"
)

//go:embed templates/*.html static/*
var embeddedFS embed.FS

// staticFS is the rooted view of the embedded static/ directory used
// by the dashboard's static asset handler. Errors from fs.Sub are
// dropped: the directory is guaranteed to exist by the //go:embed
// directive above, so a runtime failure here would indicate a build
// problem the binary cannot recover from.
var staticFS, _ = fs.Sub(embeddedFS, "static")

// loadTemplates parses every dashboard HTML template into a single
// template tree with the funcMap registered (formatBytes, pct, etc).
// Run once at handler construction time so render hot-paths skip the
// parse step.
func loadTemplates() *template.Template {
	funcMap := template.FuncMap{
		"formatBytes":  humanize.Bytes,
		"formatNumber": formatNumber,
		"pct":          pct,
		"pctFloat":     pctFloat,
		"barColor":     barColor,
		"joinStrings":  strings.Join,
	}
	return template.Must(
		template.New("").Funcs(funcMap).ParseFS(embeddedFS, "templates/*.html"),
	)
}

// formatNumber formats an integer with comma separators.
func formatNumber(n int64) string {
	if n < 0 {
		return "-" + formatNumber(-n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c)) //nolint:gosec // G115: ASCII digit rune, always fits in byte
	}
	return string(result)
}

// pct returns a formatted percentage string, or "unlimited" if limit is 0.
func pct(used, limit int64) string {
	if limit == 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%.1f%%", float64(used)/float64(limit)*100)
}

// pctFloat returns the percentage as a float for use in width styles.
func pctFloat(used, limit int64) float64 {
	if limit == 0 {
		return 0
	}
	v := float64(used) / float64(limit) * 100
	if v > 100 {
		return 100
	}
	return v
}

// barColor returns a CSS color based on the usage percentage.
func barColor(used, limit int64) string {
	if limit == 0 {
		return "#6b7280" // gray for unlimited
	}
	p := float64(used) / float64(limit) * 100
	switch {
	case p >= 90:
		return "#ef4444" // red
	case p >= 70:
		return "#f59e0b" // amber
	default:
		return "#22c55e" // green
	}
}
