package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the GitHub.com REST API root. GitHub Enterprise
	// Server uses https://HOST/api/v3/ instead; see WithBaseURL.
	DefaultBaseURL = "https://api.github.com/"

	// DefaultAPIVersion is the REST API version sent in the
	// X-GitHub-Api-Version header.
	DefaultAPIVersion = "2026-03-10"

	// DefaultUserAgent identifies this client to GitHub, which rejects
	// requests without a User-Agent.
	DefaultUserAgent = "durable-mcp (+https://github.com/nicogranuja/durable-mcp)"

	// DefaultTimeout bounds a single request, including reading the body.
	DefaultTimeout = 30 * time.Second

	// DefaultMaxResponseBytes caps a successful response body. A page of 100
	// issues with long descriptions is well under 1 MiB.
	DefaultMaxResponseBytes int64 = 10 << 20

	// maxErrorBodyBytes caps how much of an error response is read. Only the
	// JSON message is needed, so anything beyond this is dropped.
	maxErrorBodyBytes int64 = 64 << 10

	mediaType = "application/vnd.github+json"
)

// Client is a GitHub REST API client. Create one with New. A Client is safe
// for concurrent use by multiple goroutines.
type Client struct {
	httpClient *http.Client
	baseURL    *url.URL
	// authorization is the Authorization header value, or nil for an
	// unauthenticated client. It sits behind a pointer so that printing a
	// Client with fmt or slog shows an address instead of the token.
	authorization    *string
	userAgent        string
	apiVersion       string
	timeout          time.Duration
	maxResponseBytes int64
	maxPages         int
}

// Option configures a Client.
type Option func(*options)

type options struct {
	baseURL          string
	httpClient       *http.Client
	userAgent        string
	apiVersion       string
	timeout          time.Duration
	maxResponseBytes int64
	maxPages         int
}

// WithBaseURL sets the API root, for GitHub Enterprise Server
// (https://HOST/api/v3/) or for tests. The URL must be absolute.
func WithBaseURL(rawURL string) Option {
	return func(o *options) { o.baseURL = rawURL }
}

// WithHTTPClient sets the underlying HTTP client, for example to customize
// the transport. New copies it and wraps its CheckRedirect so that redirects
// cannot leave the API (see New). The client's own Timeout still applies in
// addition to the per-request timeout set by WithTimeout.
func WithHTTPClient(hc *http.Client) Option {
	return func(o *options) { o.httpClient = hc }
}

// WithUserAgent overrides DefaultUserAgent.
func WithUserAgent(ua string) Option {
	return func(o *options) { o.userAgent = ua }
}

// WithAPIVersion overrides DefaultAPIVersion, for GitHub Enterprise Server
// releases that do not support it yet.
func WithAPIVersion(v string) Option {
	return func(o *options) { o.apiVersion = v }
}

// WithTimeout sets the per-request timeout. Zero disables it, leaving only
// the caller's context deadline.
func WithTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// WithMaxResponseBytes caps the size of a successful response body. Larger
// responses fail with ErrResponseTooLarge.
func WithMaxResponseBytes(n int64) Option {
	return func(o *options) { o.maxResponseBytes = n }
}

// WithMaxPages sets how many pages a single Pager may fetch before
// NextPage fails with ErrPageLimit. The guard bounds the cost of listing a
// very large repository and stops a Link header that never ends.
func WithMaxPages(n int) Option {
	return func(o *options) { o.maxPages = n }
}

// New returns a Client that authenticates with token. An empty token makes
// unauthenticated requests, which GitHub allows for public data at a much
// lower rate limit.
//
// New refuses to send a token over plain HTTP to anything but a loopback
// address. The same rule as for pagination links applies to redirects: the
// client only follows a redirect that stays on the base URL's scheme, host,
// and path, so the token never reaches another host or a downgraded
// connection. GitHub's own redirects, such as a renamed repository moving to
// /repositories/ID, stay on the API and are followed.
func New(token string, opts ...Option) (*Client, error) {
	o := options{
		baseURL:          DefaultBaseURL,
		userAgent:        DefaultUserAgent,
		apiVersion:       DefaultAPIVersion,
		timeout:          DefaultTimeout,
		maxResponseBytes: DefaultMaxResponseBytes,
		maxPages:         DefaultMaxPages,
	}
	for _, opt := range opts {
		opt(&o)
	}

	base, err := parseBaseURL(o.baseURL, token != "")
	if err != nil {
		return nil, err
	}
	switch {
	case o.timeout < 0:
		return nil, fmt.Errorf("github: timeout must not be negative, got %s", o.timeout)
	case o.maxResponseBytes <= 0:
		return nil, fmt.Errorf("github: max response bytes must be positive, got %d", o.maxResponseBytes)
	case o.maxPages <= 0:
		return nil, fmt.Errorf("github: max pages must be positive, got %d", o.maxPages)
	case o.userAgent == "":
		return nil, errors.New("github: user agent must not be empty")
	case o.apiVersion == "":
		return nil, errors.New("github: API version must not be empty")
	}
	// Copy the caller's client so that installing CheckRedirect does not
	// change its behavior elsewhere.
	hc := &http.Client{}
	if o.httpClient != nil {
		cp := *o.httpClient
		hc = &cp
	}

	var authorization *string
	if token != "" {
		v := "Bearer " + token
		authorization = &v
	}

	c := &Client{
		httpClient:       hc,
		baseURL:          base,
		authorization:    authorization,
		userAgent:        o.userAgent,
		apiVersion:       o.apiVersion,
		timeout:          o.timeout,
		maxResponseBytes: o.maxResponseBytes,
		maxPages:         o.maxPages,
	}
	hc.CheckRedirect = c.checkRedirect(hc.CheckRedirect)
	return c, nil
}

// maxRedirects matches net/http's default redirect limit.
const maxRedirects = 10

// checkRedirect returns an http.Client.CheckRedirect that refuses redirects
// outside the API and then defers to next, or to the default limit if next
// is nil. Without it, net/http would forward the Authorization header to
// any redirect target on the same host, whatever its scheme, port, or path.
func (c *Client) checkRedirect(next func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if !c.ownsURL(req.URL) {
			return fmt.Errorf("refusing to follow redirect to %s, which is outside %s",
				req.URL.Redacted(), c.baseURL.Redacted())
		}
		if next != nil {
			return next(req, via)
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		return nil
	}
}

func parseBaseURL(rawURL string, hasToken bool) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("github: invalid base URL: %w", err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("github: base URL %q must be an absolute http(s) URL", rawURL)
	}
	if u.Scheme == "http" && hasToken && !isLoopback(u.Hostname()) {
		return nil, fmt.Errorf("github: refusing to send a token over plain HTTP to %s", u.Host)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("github: base URL %q must not contain credentials, a query, or a fragment", u.Redacted())
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	return u, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// response is what the client keeps from a successful HTTP response once
// the body has been read and the connection released.
type response struct {
	header http.Header
	body   []byte
}

// endpoint returns the absolute URL of the API path made of elem, under the
// base URL. Callers validate user-supplied elements first (see validateRepo).
func (c *Client) endpoint(elem ...string) *url.URL {
	return c.baseURL.JoinPath(elem...)
}

// get performs a GET request and returns the response if the status is 2xx.
// Any other status is converted to a *RateLimitError or an *APIError.
func (c *Client) get(ctx context.Context, u *url.URL) (*response, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("github: build request: %w", err)
	}
	req.Header.Set("Accept", mediaType)
	req.Header.Set("X-GitHub-Api-Version", c.apiVersion)
	req.Header.Set("User-Agent", c.userAgent)
	if c.authorization != nil {
		req.Header.Set("Authorization", *c.authorization)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// *url.Error repeats the method and full URL; keep just the cause.
		if uerr, ok := errors.AsType[*url.Error](err); ok {
			err = uerr.Err
		}
		return nil, fmt.Errorf("github: %s %s: %w", req.Method, u.Path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The error body is best effort: a truncated or unreadable body
		// still produces an error with the status code.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil, responseError(resp, body, time.Now())
	}

	body, err := readLimited(resp.Body, c.maxResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: read response: %w", req.Method, u.Path, err)
	}
	return &response{header: resp.Header, body: body}, nil
}

// getJSON performs a GET request and decodes the JSON response into v.
func (c *Client) getJSON(ctx context.Context, u *url.URL, v any) error {
	resp, err := c.get(ctx, u)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(resp.body, v); err != nil {
		return fmt.Errorf("github: GET %s: decode response: %w", u.Path, err)
	}
	return nil
}

// readLimited reads all of r, failing with ErrResponseTooLarge if r holds
// more than limit bytes.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w (limit %d bytes)", ErrResponseTooLarge, limit)
	}
	return body, nil
}
