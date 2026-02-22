package ui

import "testing"

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1048576, "1.0 MiB"},
		{1073741824, "1.0 GiB"},
		{1099511627776, "1.0 TiB"},
	}

	for _, tt := range tests {
		got := formatBytes(tt.input)
		if got != tt.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFormatNumber(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1,000"},
		{12345, "12,345"},
		{1000000, "1,000,000"},
		{-42, "-42"},
		{-1234, "-1,234"},
	}

	for _, tt := range tests {
		got := formatNumber(tt.input)
		if got != tt.want {
			t.Errorf("formatNumber(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestPct(t *testing.T) {
	tests := []struct {
		used, limit int64
		want        string
	}{
		{50, 100, "50.0%"},
		{1, 3, "33.3%"},
		{0, 100, "0.0%"},
		{100, 100, "100.0%"},
		{0, 0, "unlimited"},
		{500, 0, "unlimited"},
	}

	for _, tt := range tests {
		got := pct(tt.used, tt.limit)
		if got != tt.want {
			t.Errorf("pct(%d, %d) = %q, want %q", tt.used, tt.limit, got, tt.want)
		}
	}
}

func TestPctFloat(t *testing.T) {
	tests := []struct {
		used, limit int64
		want        float64
	}{
		{50, 100, 50.0},
		{0, 100, 0.0},
		{100, 100, 100.0},
		{150, 100, 100.0}, // capped at 100
		{0, 0, 0.0},       // unlimited
		{500, 0, 0.0},     // unlimited
	}

	for _, tt := range tests {
		got := pctFloat(tt.used, tt.limit)
		if got != tt.want {
			t.Errorf("pctFloat(%d, %d) = %f, want %f", tt.used, tt.limit, got, tt.want)
		}
	}
}

func TestBarColor(t *testing.T) {
	tests := []struct {
		used, limit int64
		want        string
	}{
		{50, 100, "#22c55e"},  // green (<70%)
		{69, 100, "#22c55e"},  // green (69%)
		{70, 100, "#f59e0b"},  // amber (70%)
		{85, 100, "#f59e0b"},  // amber (85%)
		{90, 100, "#ef4444"},  // red (90%)
		{100, 100, "#ef4444"}, // red (100%)
		{0, 0, "#6b7280"},    // gray (unlimited)
		{500, 0, "#6b7280"},  // gray (unlimited)
	}

	for _, tt := range tests {
		got := barColor(tt.used, tt.limit)
		if got != tt.want {
			t.Errorf("barColor(%d, %d) = %q, want %q", tt.used, tt.limit, got, tt.want)
		}
	}
}
