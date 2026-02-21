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
)

//go:embed templates/*.html static/*
var embeddedFS embed.FS

var staticFS, _ = fs.Sub(embeddedFS, "static")

func loadTemplates() *template.Template {
	funcMap := template.FuncMap{
		"formatBytes":  formatBytes,
		"formatNumber": formatNumber,
		"pct":          pct,
		"pctFloat":     pctFloat,
		"barColor":     barColor,
	}
	return template.Must(
		template.New("").Funcs(funcMap).ParseFS(embeddedFS, "templates/*.html"),
	)
}

// formatBytes converts a byte count to a human-readable string.
func formatBytes(b int64) string {
	if b == 0 {
		return "0 B"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
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
		result = append(result, byte(c))
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
