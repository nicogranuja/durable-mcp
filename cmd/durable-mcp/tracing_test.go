package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/nicogranuja/durable-mcp/internal/auth"
	"github.com/nicogranuja/durable-mcp/internal/github"
	"github.com/nicogranuja/durable-mcp/internal/mcpserver"
	"github.com/nicogranuja/durable-mcp/internal/telemetry"
)

// canary stands in for a credential. It is in no token format the span
// policy knows, so the test shows that the instrumentation never records
// it, not only that the policy scrubs it.
const canary = "canary-7f3a9c41-not-a-real-credential"

// TestSpansHoldNoCredentials sends the canary everywhere a credential
// goes, through the same span pipeline as the commands: as the bearer
// token of an MCP request that passes auth and of one that does not
// (401), in a query string, in tool arguments, and as the GitHub client's
// token, with a next-page link that carries it in its query. It then
// checks that the canary is in no span name, attribute, status, or event.
func TestSpansHoldNoCredentials(t *testing.T) {
	t.Parallel()

	exp := tracetest.NewInMemoryExporter()
	tel, err := telemetry.New(t.Context(), telemetry.Config{Component: "serve", Getenv: env(nil), SpanExporter: exp})
	if err != nil {
		t.Fatal(err)
	}

	// serve, with the canary as alice's token.
	tokens, err := auth.ParseTokenList(auth.Entry("alice", canary))
	if err != nil {
		t.Fatal(err)
	}
	server := mcpserver.New(noTemporal{}, mcpserver.Options{TracerProvider: tel.TracerProvider()})
	srv := httptest.NewServer(httpHandler(server, alwaysReady, tokens, tel, discardLogger()))
	t.Cleanup(srv.Close)

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-host"}, nil).Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + mcpPath + "?access_token=" + canary,
		HTTPClient: &http.Client{Transport: bearer{token: canary}},
	}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// A malformed job ID fails before Temporal is called.
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "job_status", Arguments: map[string]any{"job_id": canary}})
	if err != nil || !res.IsError {
		t.Fatalf("CallTool(job_status) = %+v, %v; want a tool error", res, err)
	}
	_ = cs.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+mcpPath, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer wrong-"+canary)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status with a wrong token = %d, want 401", resp.StatusCode)
	}

	// The worker's GitHub client, with the canary as its token.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+canary {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=2&access_token=%s>; rel="next"`, "http://"+r.Host, r.URL.Path, canary))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[]`)
	}))
	t.Cleanup(api.Close)
	gh, err := github.New(canary, github.WithBaseURL(api.URL+"/"),
		github.WithHTTPClient(&http.Client{Transport: tel.HTTPTransport(nil)}))
	if err != nil {
		t.Fatal(err)
	}
	pager, err := gh.ListIssues("acme", "widgets", github.IssueListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range pager.All(t.Context()) {
		if err != nil {
			t.Fatalf("list issues: %v", err)
		}
	}

	spans := exp.GetSpans()
	var serverStatuses, clientURLs []string
	toolSpans := 0
	for _, s := range spans {
		assertNoCanary(t, s)
		for _, kv := range s.Attributes {
			switch {
			case s.Name == "POST "+mcpPath && kv.Key == "http.response.status_code":
				serverStatuses = append(serverStatuses, kv.Value.String())
			case s.Name == http.MethodGet && kv.Key == "url.full":
				clientURLs = append(clientURLs, kv.Value.AsString())
			}
		}
		if s.Name == "tools/call job_status" {
			toolSpans++
		}
	}
	// The spans the test relies on exist: without them, finding no canary
	// would prove nothing.
	if !strings.Contains(strings.Join(serverStatuses, ","), "401") || !strings.Contains(strings.Join(serverStatuses, ","), "200") {
		t.Errorf("server span statuses = %v, want a 200 and a 401", serverStatuses)
	}
	if toolSpans != 1 {
		t.Errorf("got %d tool call spans, want 1", toolSpans)
	}
	if len(clientURLs) != 2 || !strings.Contains(clientURLs[1], "access_token=REDACTED") {
		t.Errorf("GitHub client span URLs = %q, want 2, the second with its access_token redacted", clientURLs)
	}
}

// assertNoCanary fails if the canary is anywhere in s.
func assertNoCanary(t *testing.T, s tracetest.SpanStub) {
	t.Helper()
	check := func(where, v string) {
		if strings.Contains(v, canary) {
			t.Errorf("span %q: %s holds the canary: %q", s.Name, where, v)
		}
	}
	check("name", s.Name)
	check("status", s.Status.Description)
	for _, kv := range s.Attributes {
		check(string(kv.Key), kv.Value.String())
	}
	for _, e := range s.Events {
		check("event", e.Name)
		for _, kv := range e.Attributes {
			check("event "+string(kv.Key), kv.Value.String())
		}
	}
	for _, l := range s.Links {
		for _, kv := range l.Attributes {
			check("link "+string(kv.Key), kv.Value.String())
		}
	}
	for _, kv := range s.Resource.Attributes() {
		check("resource "+string(kv.Key), kv.Value.String())
	}
}
