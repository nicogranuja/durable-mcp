package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/nicogranuja/durable-mcp/internal/auth"
)

// TestTokenOutputParsesBack mints a token and checks that the printed
// entry, read as the server reads its tokens, accepts the printed token as
// the named caller.
func TestTokenOutputParsesBack(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"token", "alice@example.com"}, env(nil), noStdin(), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout has %d lines, want 2:\n%s", len(lines), stdout.String())
	}
	token, ok1 := strings.CutPrefix(lines[0], "token: ")
	entry, ok2 := strings.CutPrefix(lines[1], "entry: ")
	if !ok1 || !ok2 || !strings.HasPrefix(token, auth.TokenPrefix) {
		t.Fatalf("stdout = %q, want token: and entry: lines", stdout.String())
	}
	if strings.Contains(entry, token) {
		t.Errorf("the entry %q holds the token itself", entry)
	}

	tokens, err := auth.ParseTokens(strings.NewReader(entry + "\n"))
	if err != nil {
		t.Fatalf("ParseTokens(entry): %v", err)
	}
	if c, ok := tokens.Verify(token); !ok || c.Name != "alice@example.com" {
		t.Errorf("Verify(token) = %+v, %v; want alice@example.com", c, ok)
	}

	// Each run mints a new token.
	stdout.Reset()
	if run(t.Context(), []string{"token", "alice@example.com"}, env(nil), noStdin(), &stdout, &stderr) != 0 ||
		strings.Contains(stdout.String(), token) {
		t.Errorf("a second run printed the same token, or failed: %s", stdout.String())
	}
}

func TestTokenUsageErrors(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{name: "help", args: []string{"-h"}, wantCode: 0, wantStderr: "durable-mcp token NAME"},
		{name: "no name", wantCode: 2, wantStderr: "want exactly one caller NAME"},
		{name: "two names", args: []string{"alice", "bob"}, wantCode: 2, wantStderr: "want exactly one caller NAME"},
		{name: "bad name", args: []string{"alice:x"}, wantCode: 2, wantStderr: "must be 1 to 64 ASCII letters"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), append([]string{"token"}, tt.args...), env(nil), noStdin(), &stdout, &stderr)
		if code != tt.wantCode || !strings.Contains(stderr.String(), tt.wantStderr) {
			t.Errorf("%s: exit code %d, stderr %q; want %d and %q", tt.name, code, stderr.String(), tt.wantCode, tt.wantStderr)
		}
		if stdout.Len() != 0 {
			t.Errorf("%s: stdout = %q, want nothing", tt.name, stdout.String())
		}
	}
}
