package github

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		value  string
		want   time.Duration
		wantOK bool
	}{
		{value: "", wantOK: false},
		{value: "0", want: 0, wantOK: true},
		{value: "30", want: 30 * time.Second, wantOK: true},
		{value: " 45 ", want: 45 * time.Second, wantOK: true},
		{value: "-5", wantOK: false},
		{value: "1.5", wantOK: false},
		{value: "soon", wantOK: false},
		{value: "99999999999999999", want: maxRetryAfter, wantOK: true},
		{value: now.Add(90 * time.Second).Format(http.TimeFormat), want: 90 * time.Second, wantOK: true},
		{value: now.Add(-time.Hour).Format(http.TimeFormat), want: 0, wantOK: true},
		{value: now.Add(72 * time.Hour).Format(http.TimeFormat), want: maxRetryAfter, wantOK: true},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Parallel()
			got, ok := parseRetryAfter(tt.value, now)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("parseRetryAfter(%q) = %v, %t; want %v, %t", tt.value, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
