package github

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	// DefaultMaxPages bounds how many pages one Pager fetches. At 100 items
	// per page that is 10,000 items.
	DefaultMaxPages = 100

	// maxPerPage is the largest page size GitHub accepts.
	maxPerPage = 100
)

var (
	// ErrPageLimit is returned by Pager.NextPage once the pager has fetched
	// the maximum number of pages set with WithMaxPages. The pager's Cursor
	// still points at the next page, so a caller that really needs more can
	// continue on purpose.
	ErrPageLimit = errors.New("github: page limit reached")

	// ErrInvalidCursor is returned when ListOptions.Cursor is malformed or
	// does not point at the client's API.
	ErrInvalidCursor = errors.New("github: invalid cursor")
)

// ListOptions holds the pagination parameters shared by list methods.
type ListOptions struct {
	// PerPage is the page size, from 1 to 100. Zero means 100, which keeps
	// the number of requests, and so the rate-limit cost, per item low.
	PerPage int

	// Cursor resumes a listing at the page identified by an earlier
	// Pager.Cursor. When it is set, the list method's other options are
	// ignored, because the cursor already encodes them.
	Cursor string
}

// Pager walks a paginated list endpoint one page at a time by following the
// Link header's rel="next" URL. List methods such as Client.ListIssues
// create Pagers. A Pager is not safe for concurrent use.
//
// GitHub treats next-page URLs as opaque and sometimes rewrites them, for
// example from /repos/OWNER/REPO/issues to /repositories/ID/issues, or from
// page numbers to cursors. The Pager follows them as given instead of
// building page URLs itself.
type Pager[T any] struct {
	client *Client
	next   *url.URL // nil once the last page has been fetched
	decode func([]byte) ([]T, error)
	pages  int
	rate   Rate
}

// newPager returns a Pager for the endpoint at path with query q, or one
// resumed from opts.Cursor.
func newPager[T any](c *Client, path []string, q url.Values, opts ListOptions, decode func([]byte) ([]T, error)) (*Pager[T], error) {
	p := &Pager[T]{client: c, decode: decode}

	if opts.Cursor != "" {
		u, err := c.parseCursor(opts.Cursor, path[len(path)-1])
		if err != nil {
			return nil, err
		}
		p.next = u
		return p, nil
	}

	perPage := opts.PerPage
	switch {
	case perPage == 0:
		perPage = maxPerPage
	case perPage < 0 || perPage > maxPerPage:
		return nil, fmt.Errorf("%w: per page must be between 1 and %d, got %d", ErrInvalidArgument, maxPerPage, perPage)
	}
	q.Set("per_page", strconv.Itoa(perPage))
	p.next = c.endpoint(path...)
	p.next.RawQuery = q.Encode()
	return p, nil
}

// HasNext reports whether there is another page to fetch. It is true before
// the first page.
func (p *Pager[T]) HasNext() bool {
	return p.next != nil
}

// NextPage fetches the next page and advances the pager.
//
// A page can hold fewer items than the page size, or none, when the list
// method filters results (ListIssues drops pull requests). Use HasNext, not
// the item count, to detect the end. Once HasNext is false, NextPage returns
// no items and a nil error.
//
// On error the pager does not advance, so calling NextPage again retries
// the same page. That makes it safe to retry after a *RateLimitError.
func (p *Pager[T]) NextPage(ctx context.Context) ([]T, error) {
	if p.next == nil {
		return nil, nil
	}
	if p.pages >= p.client.maxPages {
		return nil, fmt.Errorf("%w: fetched %d pages", ErrPageLimit, p.pages)
	}

	resp, err := p.client.get(ctx, p.next)
	if err != nil {
		return nil, err
	}
	items, err := p.decode(resp.body)
	if err != nil {
		return nil, fmt.Errorf("github: GET %s: decode response: %w", p.next.Path, err)
	}
	next, err := p.client.nextPageURL(resp.header, p.next)
	if err != nil {
		return nil, err
	}

	p.next = next
	p.pages++
	if rate, ok := parseRate(resp.header); ok {
		p.rate = rate
	}
	return items, nil
}

// Cursor returns an opaque string that identifies the page the next
// NextPage call would fetch, or "" if HasNext is false. Pass it as
// ListOptions.Cursor to continue the listing in another Pager, for example
// in a later Temporal activity. A cursor is only valid for a client with the
// same base URL.
func (p *Pager[T]) Cursor() string {
	if p.next == nil {
		return ""
	}
	return p.next.String()
}

// Rate returns the primary rate-limit state reported with the most recent
// page, or the zero Rate before the first page.
func (p *Pager[T]) Rate() Rate {
	return p.rate
}

// All returns an iterator over the items on all remaining pages:
//
//	for issue, err := range pager.All(ctx) {
//		if err != nil {
//			return err
//		}
//		...
//	}
//
// Iteration ends after the first error, which is yielded with the zero T.
// If the loop stops early, the rest of the current page is dropped and the
// pager stays positioned at the following page.
func (p *Pager[T]) All(ctx context.Context) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for p.HasNext() {
			items, err := p.NextPage(ctx)
			if err != nil {
				var zero T
				yield(zero, err)
				return
			}
			for _, item := range items {
				if !yield(item, nil) {
					return
				}
			}
		}
	}
}

// parseCursor validates a cursor from a caller. It must be an absolute URL
// on the client's API for the given resource (the last path segment, such as
// "issues"), so a cursor can neither send the token to another host nor
// decode one resource as another.
func (c *Client) parseCursor(cursor, resource string) (*url.URL, error) {
	u, err := url.Parse(cursor)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidCursor, err)
	}
	if u.IsAbs() {
		u = c.baseURL.ResolveReference(u) // removes "." and ".." segments
	}
	if !u.IsAbs() || !c.ownsURL(u) || !strings.HasSuffix(u.Path, "/"+resource) {
		return nil, fmt.Errorf("%w: %q is not a %s page on %s", ErrInvalidCursor, cursor, resource, c.baseURL.Redacted())
	}
	return u, nil
}

// nextPageURL returns the rel="next" URL from the response's Link header,
// resolved against the current page URL, or nil on the last page. A next
// link to any other origin is an error: following it would leak the token.
func (c *Client) nextPageURL(h http.Header, current *url.URL) (*url.URL, error) {
	link := nextLink(h.Values("Link"))
	if link == "" {
		return nil, nil
	}
	ref, err := url.Parse(link)
	if err != nil {
		return nil, fmt.Errorf("github: GET %s: invalid next page link: %w", current.Path, err)
	}
	next := current.ResolveReference(ref)
	if !c.ownsURL(next) {
		return nil, fmt.Errorf("github: GET %s: next page link %q points outside %s",
			current.Path, next.Redacted(), c.baseURL.Redacted())
	}
	return next, nil
}

// ownsURL reports whether u is on the client's API: same scheme and host as
// the base URL and under its path.
func (c *Client) ownsURL(u *url.URL) bool {
	return u.Scheme == c.baseURL.Scheme &&
		strings.EqualFold(u.Host, c.baseURL.Host) &&
		u.User == nil &&
		strings.HasPrefix(u.Path, c.baseURL.Path)
}

// nextLink returns the target of the rel="next" link in the given Link
// header values (RFC 8288), or "" if there is none.
func nextLink(values []string) string {
	for _, v := range values {
		for v != "" {
			target, params, rest, ok := cutLink(v)
			if !ok {
				break
			}
			if hasRel(params, "next") {
				return target
			}
			v = rest
		}
	}
	return ""
}

// cutLink splits the first link-value, `<target>; param=value; ...`, off a
// comma-separated Link header. Commas inside the target or inside quoted
// parameter values do not end the link-value.
func cutLink(s string) (target, params, rest string, ok bool) {
	s = strings.TrimLeft(s, " \t,")
	if !strings.HasPrefix(s, "<") {
		return "", "", "", false
	}
	end := strings.IndexByte(s, '>')
	if end < 0 {
		return "", "", "", false
	}
	target, s = s[1:end], s[end+1:]
	params, rest = cutUnquoted(s, ',')
	return target, params, rest, true
}

// hasRel reports whether link parameters include the relation type rel.
// Relation types are case-insensitive, and one rel parameter can list
// several separated by spaces.
func hasRel(params, rel string) bool {
	for params != "" {
		var param string
		param, params = cutUnquoted(params, ';')
		name, value, ok := strings.Cut(param, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "rel") {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"`)
		for r := range strings.FieldsSeq(value) {
			if strings.EqualFold(r, rel) {
				return true
			}
		}
	}
	return false
}

// cutUnquoted splits s around the first sep that is not inside a quoted
// string.
func cutUnquoted(s string, sep byte) (before, after string) {
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch {
		case inQuote && s[i] == '\\':
			i++ // skip the escaped character
		case s[i] == '"':
			inQuote = !inQuote
		case !inQuote && s[i] == sep:
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}
