package auth

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRejectionLogSamples(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	l := &rejectionLog{logger: slog.New(slog.NewTextHandler(&logs, nil)), now: func() time.Time { return now }}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil)

	l.log(req, "first")
	for range 5 {
		now = now.Add(time.Second)
		l.log(req, "suppressed")
	}
	now = now.Add(rejectionLogInterval)
	l.log(req, "after the interval")

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("logged %d lines, want 2:\n%s", len(lines), logs.String())
	}
	for i, want := range [][]string{
		{`Reason=first `, `SuppressedSinceLast=0`},
		{`Reason="after the interval" `, `SuppressedSinceLast=5`},
	} {
		for _, w := range want {
			if !strings.Contains(lines[i], w) {
				t.Errorf("line %d = %q, want it to contain %q", i+1, lines[i], w)
			}
		}
	}
}
