# Exercise 1: URL Parts Extractor — regex vs net/url

The most useful thing a regex lesson can teach first is when *not* to reach for a
regex. This module splits a URL into scheme, host, port, path, query, and
fragment — a task that looks made for a pattern and is in fact a trap. It uses
`net/url` for the parse and confines a package-level `regexp` helper to a piece
`net/url` does not split, so you feel exactly where the boundary between "real
parser" and "regex" belongs.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
urlext/                     independent module: example.com/urlext
  go.mod                    go 1.26
  urlext.go                 type Parts; Extract (net/url) + hostPathRe helper (FindStringSubmatch)
  cmd/
    demo/
      main.go               runnable demo: extract parts from a full URL
  urlext_test.go            table-driven: standard, port, empty, relative, fragment, IPv6, helper pin
```

- Files: `urlext.go`, `cmd/demo/main.go`, `urlext_test.go`.
- Implement: `Extract(raw string) (Parts, error)` using `net/url.Parse`, `u.Hostname()`, and `u.Port()`, plus a package-level `hostPathRe` regex helper `splitHostPath` demonstrating `FindStringSubmatch`.
- Test: standard URL with all parts, port stripping, empty rejection via `errors.Is(ErrEmpty)`, relative reference, fragment-only, IPv6 bracketed host, and a helper-pinning subtest suite.
- Verify: `go test -count=1 -race ./...`

### Why net/url does the parsing and the regex does almost nothing

A URL has a grammar (RFC 3986) that a regex can only approximate. Percent-encoding
(`%2F` in a path), IPv6 literals in brackets (`[::1]`), userinfo (`user:pass@`),
empty vs absent components, and port parsing are all corners a hand-rolled pattern
gets subtly wrong. `net/url.Parse` implements that grammar. So `Extract` calls it
and reads structured fields off the returned `*url.URL`: `u.Scheme`, `u.Path`,
`u.RawQuery`, `u.Fragment`. For the host it deliberately uses the two accessors
that exist precisely so you never split the authority by hand: `u.Hostname()`
strips the port *and* the IPv6 brackets, and `u.Port()` returns the port string.
That is why the IPv6 case works with no special code: `Hostname()` turns
`[::1]:8080` into `::1`.

The regex earns a cameo in `splitHostPath`, which takes a bare `host/path` string
(the kind you might pull from a config line that is not a full URL) and splits it
into the host and the path with `^([^/]+)(/.*)?$`. This is a legitimate use: the
input is genuinely irregular and there is no `net/url` accessor for it. It is also
the place to see `FindStringSubmatch` and the nil/length guard that keeps it from
panicking. The Common Mistakes section contrasts this with the *wrong* instinct —
splitting the authority with `strings.LastIndex(host, ":")`, which mangles `[::1]`.

Create `urlext.go`:

```go
package urlext

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
)

// Sentinel errors let callers branch on the failure with errors.Is.
var (
	ErrEmpty   = errors.New("empty URL")
	ErrInvalid = errors.New("invalid URL")
)

// hostPathRe splits a bare "host/path" string. It is a package-level var so the
// automaton is built once and shared; *Regexp is safe for concurrent use.
var hostPathRe = regexp.MustCompile(`^([^/]+)(/.*)?$`)

// Parts is a URL decomposed into its components.
type Parts struct {
	Scheme   string
	Host     string
	Port     string
	Path     string
	Query    string
	Fragment string
}

// Extract decomposes raw using net/url for the parse and its Hostname/Port
// accessors for the authority. It never hand-splits the host.
func Extract(raw string) (Parts, error) {
	if raw == "" {
		return Parts{}, ErrEmpty
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Parts{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return Parts{
		Scheme:   u.Scheme,
		Host:     u.Hostname(), // strips port and IPv6 brackets
		Port:     u.Port(),
		Path:     u.Path,
		Query:    u.RawQuery,
		Fragment: u.Fragment,
	}, nil
}

// splitHostPath splits a bare "host/path" string into host and path. This is the
// regex's proper job: the input is irregular and net/url has no accessor for it.
// FindStringSubmatch returns nil on no match, so guard before indexing.
func splitHostPath(hostPath string) (host, path string) {
	m := hostPathRe.FindStringSubmatch(hostPath)
	if m == nil {
		return hostPath, ""
	}
	return m[1], m[2]
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/urlext"
)

func main() {
	p, err := urlext.Extract("https://api.example.com:8443/v1/users?limit=10&page=2#results")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("scheme=%s\n", p.Scheme)
	fmt.Printf("host=%s\n", p.Host)
	fmt.Printf("port=%s\n", p.Port)
	fmt.Printf("path=%s\n", p.Path)
	fmt.Printf("query=%s\n", p.Query)
	fmt.Printf("fragment=%s\n", p.Fragment)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
scheme=https
host=api.example.com
port=8443
path=/v1/users
query=limit=10&page=2
fragment=results
```

### Tests

The table covers the shapes a real ingestion path sees: a full URL with every
component, a host:port that must split cleanly, the empty-string rejection through
`errors.Is`, a relative reference (no scheme/host), a fragment-only URL, and the
IPv6 case that proves `Hostname()` removes the brackets. The final suite pins
`splitHostPath` so a future "just use net/url everywhere" refactor has to
consciously delete the helper and its test.

Create `urlext_test.go`:

```go
package urlext

import (
	"errors"
	"fmt"
	"testing"
)

func TestExtract(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want Parts
	}{
		{
			name: "standard",
			raw:  "https://api.example.com/v1/users?limit=10&page=2#results",
			want: Parts{Scheme: "https", Host: "api.example.com", Path: "/v1/users", Query: "limit=10&page=2", Fragment: "results"},
		},
		{
			name: "host with port",
			raw:  "http://localhost:8080/healthz",
			want: Parts{Scheme: "http", Host: "localhost", Port: "8080", Path: "/healthz"},
		},
		{
			name: "relative reference",
			raw:  "/v1/users?limit=10",
			want: Parts{Path: "/v1/users", Query: "limit=10"},
		},
		{
			name: "fragment only",
			raw:  "https://example.com/#top",
			want: Parts{Scheme: "https", Host: "example.com", Path: "/", Fragment: "top"},
		},
		{
			name: "ipv6 host",
			raw:  "http://[::1]:8080/path",
			want: Parts{Scheme: "http", Host: "::1", Port: "8080", Path: "/path"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Extract(tc.raw)
			if err != nil {
				t.Fatalf("Extract(%q) error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("Extract(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestExtractRejectsEmpty(t *testing.T) {
	t.Parallel()
	if _, err := Extract(""); !errors.Is(err, ErrEmpty) {
		t.Fatalf("Extract(\"\") err = %v, want ErrEmpty", err)
	}
}

func TestExtractRejectsInvalid(t *testing.T) {
	t.Parallel()
	// A control character in the host makes url.Parse fail.
	if _, err := Extract("http://exa\x7fmple.com"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestSplitHostPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in       string
		wantHost string
		wantPath string
	}{
		{in: "example.com", wantHost: "example.com", wantPath: ""},
		{in: "example.com/", wantHost: "example.com", wantPath: "/"},
		{in: "example.com/path/to/page", wantHost: "example.com", wantPath: "/path/to/page"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			host, path := splitHostPath(tc.in)
			if host != tc.wantHost || path != tc.wantPath {
				t.Fatalf("splitHostPath(%q) = (%q,%q), want (%q,%q)", tc.in, host, path, tc.wantHost, tc.wantPath)
			}
		})
	}
}

func ExampleExtract() {
	p, _ := Extract("https://example.com:443/a?b=c#d")
	fmt.Printf("%s %s %s %s %s %s\n", p.Scheme, p.Host, p.Port, p.Path, p.Query, p.Fragment)
	// Output: https example.com 443 /a b=c d
}
```

## Review

`Extract` is correct when every field comes from a `net/url` accessor and none is
hand-parsed: `Hostname()` strips the port and IPv6 brackets, `Port()` returns the
port, and the IPv6 test proves it by turning `[::1]:8080` into `::1` with no
special case. The regex is confined to `splitHostPath`, where the input is
genuinely irregular and the `if m == nil` guard keeps `FindStringSubmatch` from
panicking on a non-match. The two failure modes to remember are the ones the
tests pin: never validate a URL with a bare `^https?://` pattern (it misses
percent-encoding and IPv6 — use `url.Parse` and check `u.Scheme`/`u.Host`), and
never split the authority with `strings.LastIndex(host, ":")`, which cuts `[::1]`
in the wrong place. Run `go test -race` to confirm the shared package-level regex
is safe under concurrent extraction.

## Resources

- [`net/url` package](https://pkg.go.dev/net/url) — `Parse`, and the `Hostname`/`Port` accessors that split the authority for you.
- [`regexp` package](https://pkg.go.dev/regexp) — `MustCompile` and `FindStringSubmatch`.
- [RFC 3986: Uniform Resource Identifier](https://www.rfc-editor.org/rfc/rfc3986) — the grammar `net/url` implements and a regex only approximates.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-structured-log-line-parser.md](02-structured-log-line-parser.md)
