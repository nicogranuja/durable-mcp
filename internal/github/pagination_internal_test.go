package github

import "testing"

func TestNextLink(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{name: "no header", values: nil, want: ""},
		{name: "empty header", values: []string{""}, want: ""},
		{
			name:   "GitHub style",
			values: []string{`<https://api.github.com/repositories/1/issues?page=2>; rel="next", <https://api.github.com/repositories/1/issues?page=5>; rel="last"`},
			want:   "https://api.github.com/repositories/1/issues?page=2",
		},
		{
			name:   "next is not first",
			values: []string{`<https://x.test/a?page=1>; rel="prev", <https://x.test/a?page=3>; rel="next"`},
			want:   "https://x.test/a?page=3",
		},
		{
			name:   "last page has no next",
			values: []string{`<https://x.test/a?page=1>; rel="first", <https://x.test/a?page=4>; rel="prev"`},
			want:   "",
		},
		{
			name:   "comma inside the URL",
			values: []string{`<https://x.test/a?labels=bug,ui&page=2>; rel="next"`},
			want:   "https://x.test/a?labels=bug,ui&page=2",
		},
		{
			name:   "several relation types",
			values: []string{`<https://x.test/a?page=2>; rel="last next"`},
			want:   "https://x.test/a?page=2",
		},
		{
			name:   "unquoted rel",
			values: []string{`<https://x.test/a?page=2>; rel=next`},
			want:   "https://x.test/a?page=2",
		},
		{
			name:   "case-insensitive",
			values: []string{`<https://x.test/a?page=2>; REL="Next"`},
			want:   "https://x.test/a?page=2",
		},
		{
			name:   "separators inside a quoted parameter",
			values: []string{`<https://x.test/a?page=1>; title="a, b; rel=next"; rel="prev", <https://x.test/a?page=3>; rel="next"`},
			want:   "https://x.test/a?page=3",
		},
		{
			name:   "escaped quote inside a quoted parameter",
			values: []string{`<https://x.test/a?page=1>; title="say \"hi\", rel=next"; rel="prev", <https://x.test/a?page=3>; rel="next"`},
			want:   "https://x.test/a?page=3",
		},
		{
			name:   "next in a second header line",
			values: []string{`<https://x.test/a?page=1>; rel="prev"`, `<https://x.test/a?page=3>; rel="next"`},
			want:   "https://x.test/a?page=3",
		},
		{
			name:   "extra whitespace",
			values: []string{"  <https://x.test/a?page=2> ;\trel = \"next\" "},
			want:   "https://x.test/a?page=2",
		},
		{
			name:   "rel-like parameter name",
			values: []string{`<https://x.test/a?page=2>; xrel="next"`},
			want:   "",
		},
		{name: "missing angle brackets", values: []string{`https://x.test/a?page=2; rel="next"`}, want: ""},
		{name: "unterminated target", values: []string{`<https://x.test/a?page=2; rel="next"`}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := nextLink(tt.values); got != tt.want {
				t.Errorf("nextLink(%q) = %q, want %q", tt.values, got, tt.want)
			}
		})
	}
}
