// Package auth authenticates callers of the MCP server's HTTP transport
// with bearer tokens, and carries the caller's identity in a context.
//
// # Tokens
//
// The server is configured with [Tokens]: a list of name:hash entries, one
// per caller, where hash is the hex SHA-256 of the caller's token. The
// server never holds the tokens themselves, so a leaked configuration file
// does not let anyone in. [NewToken] mints a token, and [Entry] makes its
// entry.
//
// A plain SHA-256, rather than a slow password hash such as bcrypt, is
// enough because the tokens are random and long (130 bits from
// crypto/rand): there is no dictionary to guess from, and a fast hash keeps
// each request cheap to check. A token chosen by a person would need a slow
// hash, so tokens should always come from NewToken.
//
// [Tokens.Verify] hashes the presented token and compares the hash with
// every entry in constant time, without stopping at a match, so the time it
// takes does not depend on which entry matched or how much of a hash did.
//
// # Callers
//
// A caller is identified by the entry's name, not by its token, so a token
// can be rotated (give the name a new token and hash, and restart) without
// changing who owns which jobs. [Middleware] puts the [Caller] in the
// request's context, and hands it to the MCP SDK as the request's token
// info, from which the MCP server reads it.
package auth

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// TokenPrefix starts every token that NewToken mints, so that secret
// scanners and people can recognize one.
const TokenPrefix = "dmcp_"

// maxNameLength bounds a caller's name, which is stored in every job the
// caller starts.
const maxNameLength = 64

// Caller is an authenticated caller.
type Caller struct {
	// Name is the caller's name from its token entry.
	Name string
}

type callerKey struct{}

// WithCaller returns a copy of ctx that carries c.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom returns the caller in ctx, if there is one.
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// Tokens is a set of callers and the SHA-256 hashes of their tokens. The
// zero value has no callers.
type Tokens struct {
	entries []entry
}

type entry struct {
	name string
	hash [sha256.Size]byte
}

// Len returns the number of callers.
func (t *Tokens) Len() int { return len(t.entries) }

// Names returns the callers' names, in the order they were configured.
func (t *Tokens) Names() []string {
	names := make([]string, len(t.entries))
	for i, e := range t.entries {
		names[i] = e.name
	}
	return names
}

// Verify returns the caller whose token is token. It compares token's hash
// with every entry in constant time; see the package documentation.
func (t *Tokens) Verify(token string) (Caller, bool) {
	sum := sha256.Sum256([]byte(token))
	match := -1
	for i, e := range t.entries {
		eq := subtle.ConstantTimeCompare(sum[:], e.hash[:])
		match = subtle.ConstantTimeSelect(eq, i, match)
	}
	if match < 0 {
		return Caller{}, false
	}
	return Caller{Name: t.entries[match].name}, true
}

// ParseTokens reads token entries, one per line, as "name:hash". Blank
// lines and lines starting with '#' are ignored. Names must be unique, and
// so must hashes: two callers with the same token could not be told apart.
//
// Errors name the line, but never echo a hash.
func ParseTokens(r io.Reader) (*Tokens, error) {
	t := &Tokens{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := t.add(line); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return t, nil
}

// ParseTokenList reads token entries separated by commas or whitespace, as
// in an environment variable: "alice:HASH,bob:HASH".
func ParseTokenList(s string) (*Tokens, error) {
	t := &Tokens{}
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
	for i, f := range fields {
		if err := t.add(f); err != nil {
			return nil, fmt.Errorf("entry %d: %w", i+1, err)
		}
	}
	return t, nil
}

func (t *Tokens) add(s string) error {
	name, hexHash, ok := strings.Cut(s, ":")
	if !ok {
		return errors.New(`want "name:sha256-hex-of-token"`)
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	if strings.HasPrefix(hexHash, TokenPrefix) {
		return fmt.Errorf("the entry for %q holds a token, not its hash: store the hex SHA-256 of the token", name)
	}
	var e entry
	if len(hexHash) != 2*sha256.Size {
		return fmt.Errorf("the hash for %q must be %d hex characters, the SHA-256 of the token", name, 2*sha256.Size)
	}
	if _, err := hex.Decode(e.hash[:], []byte(hexHash)); err != nil {
		return fmt.Errorf("the hash for %q is not hex", name)
	}
	if e.hash == sha256.Sum256(nil) {
		return fmt.Errorf("the hash for %q is the hash of an empty token", name)
	}
	e.name = name
	for _, other := range t.entries {
		switch {
		case other.name == name:
			return fmt.Errorf("caller %q is listed twice", name)
		case other.hash == e.hash:
			return fmt.Errorf("callers %q and %q have the same token; give each caller its own", other.name, name)
		}
	}
	t.entries = append(t.entries, e)
	return nil
}

// ValidateName checks a caller's name: 1 to 64 ASCII letters, digits, '.',
// '_', '-', and '@'.
func ValidateName(name string) error {
	if name == "" || len(name) > maxNameLength || strings.ContainsFunc(name, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("._-@", r)
	}) {
		return fmt.Errorf("caller name %q must be 1 to %d ASCII letters, digits, '.', '_', '-', or '@'",
			truncate(name, maxNameLength+1), maxNameLength)
	}
	return nil
}

// NewToken returns a new random token: TokenPrefix and 26 base32
// characters, which encode 130 bits from crypto/rand.
func NewToken() string {
	return TokenPrefix + strings.ToLower(rand.Text())
}

// Hash returns the hex SHA-256 of token, as a token entry stores it.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Entry returns the token entry for a caller with the given name and
// token.
func Entry(name, token string) string {
	return name + ":" + Hash(token)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "..."
}
