package mcpserver

import (
	"context"
	"testing"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nicogranuja/durable-mcp/internal/auth"
)

func TestWithCaller(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		extra    *mcp.RequestExtra
		wantName string
		wantOK   bool
	}{
		{name: "token info", extra: &mcp.RequestExtra{TokenInfo: &mcpauth.TokenInfo{UserID: "alice"}}, wantName: "alice", wantOK: true},
		{name: "no extra, as over stdio", extra: nil},
		{name: "no token info, as over HTTP without auth", extra: &mcp.RequestExtra{}},
		{name: "empty user ID", extra: &mcp.RequestExtra{TokenInfo: &mcpauth.TokenInfo{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got auth.Caller
			var ok bool
			h := withCaller(func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
				got, ok = auth.CallerFrom(ctx)
				return nil, nil
			})
			if _, err := h(t.Context(), "tools/call", &mcp.CallToolRequest{Extra: tt.extra}); err != nil {
				t.Fatal(err)
			}
			if got.Name != tt.wantName || ok != tt.wantOK {
				t.Errorf("caller = %+v, %v; want %q, %v", got, ok, tt.wantName, tt.wantOK)
			}
		})
	}
}
