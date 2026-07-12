# Exercise 9: Normalize an Endpoint in the Constructor so Equal Inputs Produce Equal Keys

A connection pool keyed by endpoint and a cache keyed by URL both depend on one
property: logically-equal endpoints must compare and hash equal. `HTTP://Example.COM:443/`
and `https://example.com` are the same endpoint, but as raw strings they are
distinct keys that silently double connections and miss cache. This exercise
builds `NewEndpoint` that canonicalizes at construction so downstream code never
re-normalizes.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
endpoint/                    independent module: example.com/normalize-endpoint-canonical
  go.mod
  endpoint.go                Endpoint (comparable), NewEndpoint canonicalizer, String, sentinels
  cmd/
    demo/
      main.go                normalizes several spellings, shows they collapse to one key
  endpoint_test.go           equal spellings collapse, invalid/unsupported, idempotent, map-key identity
```

- Files: `endpoint.go`, `cmd/demo/main.go`, `endpoint_test.go`.
- Implement: `NewEndpoint(raw string) (Endpoint, error)` that defaults the scheme, lowercases the host, strips the default port, trims the trailing slash, and rejects invalid URLs and unsupported schemes.
- Test: distinct spellings of one endpoint construct to an equal canonical string; invalid URLs and unsupported schemes return sentinels; the canonical form is idempotent; and two equal endpoints work as identical map keys.
- Verify: `go test -count=1 -race ./...`

### Normalization is a constructor's job

The equality a cache or pool relies on is not a property of the raw input — it is
a property you must impose. The constructor is the place to impose it, once, so
that every downstream comparison and every map lookup gets it for free. The
canonical form here folds together the ways the same endpoint can be spelled: a
missing scheme defaults to `https`; the host is lowercased (hostnames are
case-insensitive); the default port for the scheme (`443` for https, `80` for
http) is stripped because `example.com:443` and `example.com` are the same
authority under https; and a trailing slash on the path is trimmed. IP-literal
hosts are canonicalized through `net/netip`, which compresses IPv6 (`2001:db8::0001`
becomes `2001:db8::1`) so two spellings of one address collapse too.

The `Endpoint` stores only the canonical string in an unexported field, which
makes the type `comparable` — two `Endpoint` values are `==` exactly when their
canonical forms match — so it can be a map key directly. Canonicalization is
idempotent: feeding a canonical string back through `NewEndpoint` returns the same
value, which is the property a test pins, because a non-idempotent normalizer
would produce different keys on a second pass and reintroduce the exact bug it was
meant to prevent. Unsupported schemes and unparseable URLs are rejected with
sentinels, because an endpoint the pool cannot dial is not a valid key.

Create `endpoint.go`:

```go
package endpoint

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

var (
	ErrInvalidURL        = errors.New("invalid endpoint URL")
	ErrUnsupportedScheme = errors.New("unsupported scheme")
)

// Endpoint is a canonicalized network endpoint. It holds only its canonical
// string, so it is comparable and usable directly as a map key.
type Endpoint struct {
	canonical string
}

// NewEndpoint parses raw and returns its canonical form: scheme defaulted to
// https, host lowercased, default port stripped, trailing slash trimmed.
func NewEndpoint(raw string) (Endpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Endpoint{}, fmt.Errorf("%w: empty", ErrInvalidURL)
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return Endpoint{}, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return Endpoint{}, fmt.Errorf("%w: %q", ErrUnsupportedScheme, scheme)
	}

	host := strings.ToLower(u.Hostname())
	if host == "" {
		return Endpoint{}, fmt.Errorf("%w: missing host", ErrInvalidURL)
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Is6() {
			host = "[" + addr.String() + "]"
		} else {
			host = addr.String()
		}
	}

	authority := host
	if port := u.Port(); port != "" && !isDefaultPort(scheme, port) {
		authority = host + ":" + port
	}

	path := strings.TrimSuffix(u.Path, "/")
	canonical := scheme + "://" + authority + path
	return Endpoint{canonical: canonical}, nil
}

func isDefaultPort(scheme, port string) bool {
	return (scheme == "http" && port == "80") || (scheme == "https" && port == "443")
}

// String returns the canonical endpoint.
func (e Endpoint) String() string { return e.canonical }
```

### The runnable demo

The demo normalizes several spellings of the same endpoint and shows they all
collapse to one canonical key, then normalizes a distinct endpoint for contrast.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/normalize-endpoint-canonical"
)

func main() {
	spellings := []string{
		"https://api.example.com",
		"HTTPS://API.Example.COM:443/",
		"api.example.com/",
	}
	for _, s := range spellings {
		e, _ := endpoint.NewEndpoint(s)
		fmt.Printf("%-34s -> %s\n", s, e)
	}

	other, _ := endpoint.NewEndpoint("http://cache.internal:8080/v1")
	fmt.Printf("%-34s -> %s\n", "http://cache.internal:8080/v1", other)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
https://api.example.com            -> https://api.example.com
HTTPS://API.Example.COM:443/       -> https://api.example.com
api.example.com/                   -> https://api.example.com
http://cache.internal:8080/v1      -> http://cache.internal:8080/v1
```

### Tests

`TestSpellingsCollapse` is the core property: several spellings of one endpoint
produce an equal canonical string. `TestMapKeyIdentity` proves the payoff — two
equal endpoints index the same map entry. `TestIdempotent` feeds the canonical
form back through the constructor and asserts it is a fixed point.

Create `endpoint_test.go`:

```go
package endpoint

import (
	"errors"
	"fmt"
	"testing"
)

func TestSpellingsCollapse(t *testing.T) {
	t.Parallel()
	spellings := []string{
		"https://api.example.com",
		"HTTPS://API.Example.COM:443/",
		"api.example.com/",
		"https://api.example.com:443",
	}
	want := "https://api.example.com"
	for _, s := range spellings {
		e, err := NewEndpoint(s)
		if err != nil {
			t.Fatalf("NewEndpoint(%q) error: %v", s, err)
		}
		if e.String() != want {
			t.Fatalf("NewEndpoint(%q) = %q, want %q", s, e.String(), want)
		}
	}
}

func TestIPv6Compressed(t *testing.T) {
	t.Parallel()
	e, err := NewEndpoint("https://[2001:db8::0001]:443/api/")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.String(); got != "https://[2001:db8::1]/api" {
		t.Fatalf("IPv6 canonical = %q", got)
	}
}

func TestInvalidAndUnsupported(t *testing.T) {
	t.Parallel()
	if _, err := NewEndpoint(""); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("empty err = %v, want ErrInvalidURL", err)
	}
	if _, err := NewEndpoint("ftp://files.example.com"); !errors.Is(err, ErrUnsupportedScheme) {
		t.Fatalf("ftp err = %v, want ErrUnsupportedScheme", err)
	}
	if _, err := NewEndpoint("https://%zz"); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("bad url err = %v, want ErrInvalidURL", err)
	}
}

func TestIdempotent(t *testing.T) {
	t.Parallel()
	first, err := NewEndpoint("HTTPS://API.Example.COM:443/")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewEndpoint(first.String())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("not idempotent: %q vs %q", first, second)
	}
}

func TestMapKeyIdentity(t *testing.T) {
	t.Parallel()
	pool := map[Endpoint]int{}
	a, _ := NewEndpoint("https://api.example.com")
	b, _ := NewEndpoint("HTTPS://api.example.com:443/")
	pool[a]++
	pool[b]++
	if len(pool) != 1 {
		t.Fatalf("equal endpoints produced %d keys, want 1", len(pool))
	}
	if pool[a] != 2 {
		t.Fatalf("count = %d, want 2", pool[a])
	}
}

func ExampleNewEndpoint() {
	e, _ := NewEndpoint("HTTPS://API.Example.COM:443/")
	fmt.Println(e)
	// Output: https://api.example.com
}
```

## Review

The endpoint is correct when every spelling of one endpoint collapses to a single
canonical string, that string is a fixed point of the constructor, and two equal
endpoints index one map entry. The design point is that normalization is not the
caller's job to remember — it lives in the constructor and is encoded in a
comparable type, so a pool or cache gets correct keying for free. The mistake to
avoid is skipping normalization and keying on raw strings, which silently doubles
connections and halves cache hits; the idempotence test is what proves the
normalizer will not itself reintroduce distinct keys on a second pass.

## Resources

- [net/url.Parse](https://pkg.go.dev/net/url#Parse) — parsing the raw endpoint; `URL.Hostname`/`URL.Port` split the authority.
- [net/netip.ParseAddr](https://pkg.go.dev/net/netip#ParseAddr) — canonicalizing IP-literal hosts, including IPv6 compression.
- [RFC 3986 §6: Normalization and Comparison](https://www.rfc-editor.org/rfc/rfc3986#section-6) — the URI equivalence rules this applies.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-builder-accumulate-errors.md](08-builder-accumulate-errors.md) | Next: [10-must-be-constructed-guard.md](10-must-be-constructed-guard.md)
