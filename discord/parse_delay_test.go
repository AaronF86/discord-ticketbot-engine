package discord

import (
	"testing"
	"time"
)

func TestParseDelay(t *testing.T) {
	const day = 24 * time.Hour
	tests := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"1h", time.Hour, true},
		{"90m", 90 * time.Minute, true},
		{"2d", 2 * day, true},
		{"1w", 7 * day, true},
		{"1.5d", 36 * time.Hour, true},
		{"1w 2d 3h", 9*day + 3*time.Hour, true},
		{"1d12h30m", day + 12*time.Hour + 30*time.Minute, true},
		{"2d 2d", 4 * day, true},
		{"", 0, false},
		{"0h", 0, false},
		{"d", 0, false},
		{"xd", 0, false},
		{"abc", 0, false},
		{"-1h", 0, false},
		{"1d-1h", day - time.Hour, true},
	}
	for _, tt := range tests {
		got, ok := parseDelay(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseDelay(%q) = %v, %v; want %v, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
