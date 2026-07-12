# Exercise 28: API Base URLs Parsed, Validated, and Cached at init for Multiple Service Endpoints

**Nivel: Intermedio** — validacion rapida (un test corto).

A service that talks to several downstream APIs — users, billing, search —
typically has one base URL per service, known at deploy time and unchanging
for the life of the process. Calling `url.Parse` on those strings freshly
for every outgoing request wastes work and defers a malformed URL's failure
to whichever request happens to trigger it first. This exercise parses and
validates every base URL once, at package initialization, caching the
parsed `*url.URL` so building a request endpoint is a cheap path join
against an already-known-good value.

## What you'll build

```text
apiurls/                   independent module: example.com/apiurls
  go.mod                    module example.com/apiurls
  apiurls.go                  parseBaseURLs validation + cached *url.URL map + Join
  cmd/
    demo/
      main.go                 builds two endpoints, shows the unknown-service and copy-safety cases
  apiurls_test.go              parseBaseURLs validation table + Join test + copy-safety test
```

Files: `apiurls.go`, `cmd/demo/main.go`, `apiurls_test.go`.
Implement: `parseBaseURLs(raw map[string]string) (map[string]*url.URL, error)` requiring an `https` scheme, a non-empty host, and no query string, and trimming a trailing slash from the path; `BaseURL(service string) (*url.URL, bool)` returning an independent copy of the cached URL; `Join(service, path string) (string, error)` building a full endpoint string.
Test: `parseBaseURLs` accepts a valid https URL and rejects a malformed URL, a non-https scheme, a missing host, and a URL with a query string; `Join` builds the expected endpoint and errors on an unknown service; a caller mutating the `*url.URL` returned by `BaseURL` never affects the cached original.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/08-init-functions-and-package-initialization/28-api-base-url-parser-and-validator/cmd/demo
cd go-solutions/04-functions/08-init-functions-and-package-initialization/28-api-base-url-parser-and-validator
go mod edit -go=1.24
```

### Why parse once, and why return a copy

`url.Parse` is not free — it walks the string, splits it into scheme, host,
path, and query, and allocates a `*url.URL`. For a base URL that is static
configuration, parsing it once at init and reusing the result for the
process's entire lifetime is strictly better than parsing it on every
outgoing request: the cost is paid exactly once, and a malformed
configuration value — a typo'd scheme, a URL that slipped in an unwanted
query string — fails loudly the instant the binary starts, not on whichever
request happens to be unlucky enough to trigger the parse first.

The validation rules here are deliberately stricter than "does `url.Parse`
return an error": `url.Parse` happily accepts `"http://..."`  and
`"https://host?x=1"`, neither of which is an acceptable *base* URL for this
system — the former is not encrypted, the latter would silently smuggle a
query parameter into every request built from it. `parseBaseURLs` rejects
both explicitly, alongside a missing host, and normalizes a trailing slash
so `Join` never has to worry about a double slash or a missing one between
base and path.

`BaseURL` returns a copy of the cached `*url.URL`, never the cached pointer
itself. `url.URL` is a plain struct, so `cp := *u; return &cp` is a full,
independent copy for this shape of data (no nested pointer fields are used
here). Returning the cached pointer directly would let one caller's
`u.Path = "/something-else"` corrupt the shared cache for every other
caller — exactly the kind of action-at-a-distance bug a cache of shared,
supposedly-immutable configuration must not allow.

Create `apiurls.go`:

```go
// apiurls.go
// Package apiurls parses and validates a set of API base URLs, one per
// downstream service, at package initialization -- caching the parsed
// *url.URL for each service so that building an endpoint at request time is
// a cheap join against an already-validated URL, never a fresh url.Parse
// (and never a fresh chance to fail on a malformed string) per request.
package apiurls

import (
	"fmt"
	"net/url"
	"strings"
)

// rawBaseURLs is the static configuration: one base URL per service name.
var rawBaseURLs = map[string]string{
	"users":   "https://api.example.com/users",
	"billing": "https://billing.example.com/v2",
	"search":  "https://search.example.com",
}

// parsed caches each service's validated, parsed base URL, built once at
// init from rawBaseURLs.
var parsed map[string]*url.URL

func init() {
	p, err := parseBaseURLs(rawBaseURLs)
	if err != nil {
		panic("apiurls: " + err.Error())
	}
	parsed = p
}

// parseBaseURLs validates and parses every entry in raw, requiring an
// https scheme, a non-empty host, and no query string (a base URL is not
// meant to carry query parameters). It is extracted from init so tests can
// exercise malformed configuration directly.
func parseBaseURLs(raw map[string]string) (map[string]*url.URL, error) {
	out := make(map[string]*url.URL, len(raw))
	for service, base := range raw {
		u, err := url.Parse(base)
		if err != nil {
			return nil, fmt.Errorf("service %q: malformed base URL %q: %w", service, base, err)
		}
		if u.Scheme != "https" {
			return nil, fmt.Errorf("service %q: base URL %q must use https, got %q", service, base, u.Scheme)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("service %q: base URL %q has no host", service, base)
		}
		if u.RawQuery != "" {
			return nil, fmt.Errorf("service %q: base URL %q must not have a query string", service, base)
		}
		u.Path = strings.TrimSuffix(u.Path, "/")
		out[service] = u
	}
	return out, nil
}

// BaseURL returns a copy of the parsed base URL for service, so a caller
// can never mutate the cached original.
func BaseURL(service string) (*url.URL, bool) {
	u, ok := parsed[service]
	if !ok {
		return nil, false
	}
	cp := *u
	return &cp, true
}

// Join builds a full endpoint URL string for service by appending path to
// its cached base URL.
func Join(service, path string) (string, error) {
	base, ok := BaseURL(service)
	if !ok {
		return "", fmt.Errorf("apiurls: unknown service %q", service)
	}
	base.Path += "/" + strings.TrimPrefix(path, "/")
	return base.String(), nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/apiurls"
)

func main() {
	endpoint, err := apiurls.Join("users", "/42/profile")
	if err != nil {
		fmt.Println("error:", err)
	} else {
		fmt.Println("users endpoint:", endpoint)
	}

	endpoint, err = apiurls.Join("billing", "invoices")
	if err != nil {
		fmt.Println("error:", err)
	} else {
		fmt.Println("billing endpoint:", endpoint)
	}

	_, err = apiurls.Join("unknown-service", "ping")
	fmt.Println("unknown service error:", err)

	base, _ := apiurls.BaseURL("users")
	base.Path = "/mutated"
	cached, _ := apiurls.BaseURL("users")
	fmt.Println("cached base path unaffected by caller mutation:", cached.Path)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
users endpoint: https://api.example.com/users/42/profile
billing endpoint: https://billing.example.com/v2/invoices
unknown service error: apiurls: unknown service "unknown-service"
cached base path unaffected by caller mutation: /users
```

### Tests

Create `apiurls_test.go`:

```go
// apiurls_test.go
package apiurls

import (
	"strings"
	"testing"
)

func TestParseBaseURLs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     map[string]string
		wantErr string
	}{
		{"valid https", map[string]string{"a": "https://api.example.com/v1"}, ""},
		{"trailing slash trimmed", map[string]string{"a": "https://api.example.com/v1/"}, ""},
		{"malformed url", map[string]string{"a": "://bad"}, "malformed"},
		{"wrong scheme", map[string]string{"a": "http://api.example.com"}, "must use https"},
		{"no host", map[string]string{"a": "https:///v1"}, "no host"},
		{"has query", map[string]string{"a": "https://api.example.com?x=1"}, "query string"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseBaseURLs(tc.raw)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got["a"] == nil {
					t.Fatal("expected a parsed URL for service \"a\"")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestJoinBuildsEndpoint(t *testing.T) {
	t.Parallel()

	got, err := Join("users", "/42/profile")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "https://api.example.com/users/42/profile"
	if got != want {
		t.Fatalf("Join = %q, want %q", got, want)
	}

	if _, err := Join("nonexistent", "x"); err == nil {
		t.Fatal("expected error for unknown service")
	}
}

func TestBaseURLReturnsIndependentCopy(t *testing.T) {
	t.Parallel()

	u, ok := BaseURL("billing")
	if !ok {
		t.Fatal("BaseURL(\"billing\") ok = false")
	}
	original := u.Path
	u.Path = "/mutated"

	u2, _ := BaseURL("billing")
	if u2.Path != original {
		t.Fatalf("cached base URL was mutated: got path %q, want %q", u2.Path, original)
	}
}
```

## Review

`parseBaseURLs` is correct when it rejects every documented shape of bad
configuration — a string `url.Parse` itself rejects, a non-https scheme, a
missing host, an unwanted query string — and accepts a well-formed https
URL, trimming its trailing slash so `Join` never has to special-case it.
`TestBaseURLReturnsIndependentCopy` is the copy-safety proof: mutating the
`*url.URL` a caller received back from `BaseURL` must never be visible to
the next caller of `BaseURL` for the same service, because the cache holds
one shared value that every caller reads.

The mistake to avoid is returning the cached `*url.URL` pointer directly.
It looks harmless — most callers only read from it — but the type is
mutable, and the one caller that does `u.Path += "/x"` in place corrupts
the shared cache for every subsequent caller, a bug that only shows up once
two different call sites touch the same service's base URL in the same
process, which is exactly the situation a multi-endpoint client has by
construction.

## Resources

- [net/url](https://pkg.go.dev/net/url) — `url.Parse` and the `URL` struct this package caches.
- [Go spec: Package initialization](https://go.dev/ref/spec#Package_initialization) — why parsing and validating configuration belongs in `init()`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-request-correlation-id-entropy-generator.md](27-request-correlation-id-entropy-generator.md) | Next: [29-dns-cache-prewarming-with-resolver.md](29-dns-cache-prewarming-with-resolver.md)
