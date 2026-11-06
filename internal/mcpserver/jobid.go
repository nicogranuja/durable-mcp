package mcpserver

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nicogranuja/durable-mcp/internal/report"
)

// A job ID is the workflow ID of the job's RepositoryReport workflow:
// jobIDPrefix followed by 26 base32 characters, which encode 128 or more
// bits. A random ID comes from crypto/rand; an ID for an idempotency key is
// a hash of the key, so the same key always names the same job.
const (
	jobIDPrefix  = "report-"
	jobIDLen     = len(jobIDPrefix) + 26
	maxKeyLength = 200
)

// idEncoding is lowercase base32 without padding. Lowercase reads better
// in a workflow ID and matches the case of rand.Text after strings.ToLower.
var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// newJobID returns a random job ID.
func newJobID() string {
	return jobIDPrefix + strings.ToLower(rand.Text()) // 26 characters, 130 bits
}

// jobIDForKey returns the job ID that an idempotency key maps to: the
// first 128 bits of a SHA-256 hash of the key. Hashing keeps the ID a fixed
// length and charset whatever the key is, and keeps the key's text out of
// the workflow ID, which shows in the Temporal UI and logs.
func jobIDForKey(key string) string {
	sum := sha256.Sum256([]byte("durable-mcp report job\x00" + key))
	return jobIDPrefix + idEncoding.EncodeToString(sum[:16])
}

// validateJobID checks that id has the shape of a job ID before it is sent
// to Temporal.
func validateJobID(id string) error {
	if len(id) != jobIDLen || !strings.HasPrefix(id, jobIDPrefix) ||
		strings.Trim(id[len(jobIDPrefix):], "abcdefghijklmnopqrstuvwxyz234567") != "" {
		return fmt.Errorf("%q is not a job ID: a job ID looks like %q and comes from start_report. "+
			"Use the job_id that start_report returned", truncate(id, 80), jobIDPrefix+strings.Repeat("x", 26))
	}
	return nil
}

// validateKey checks an idempotency key.
func validateKey(key string) error {
	if !utf8.ValidString(key) || utf8.RuneCountInString(key) > maxKeyLength ||
		strings.ContainsFunc(key, unicode.IsControl) {
		return fmt.Errorf("idempotency_key must be at most %d characters of printable text, such as a UUID", maxKeyLength)
	}
	return nil
}

// fingerprint identifies the request a job was started for, so that a
// retry with the same idempotency key can be checked against it. GitHub
// names are not case sensitive, so neither is the fingerprint.
func fingerprint(req report.Request) string {
	canonical := report.Request{
		Owner:    strings.ToLower(req.Owner),
		Repo:     strings.ToLower(req.Repo),
		Since:    req.Since.UTC(),
		Until:    req.Until.UTC(),
		MaxPages: req.MaxPages,
	}
	b, err := json.Marshal(canonical)
	if err != nil {
		panic(err) // a Request always marshals
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// truncate shortens s to at most n runes, for echoing input in errors.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}
