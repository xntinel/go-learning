# Exercise 9: Build a Canonical Query String for HMAC Request Signing

Request signing (webhooks, API gateways, cloud SDKs) hinges on both peers computing
the exact same string-to-sign. For query parameters that means a canonical form:
percent-encoded, key-sorted, joined deterministically, then fed to HMAC-SHA256. This
exercise builds that canonical string, compares a manual `strings.Builder` version
against `url.Values.Encode`, and shows why encoding correctness is a security concern,
not a cosmetic one.

This module is self-contained.

## What you'll build

```text
qsign/                       independent module: example.com/qsign
  go.mod
  qsign.go                   Canonical (Builder + QueryEscape + slices.Sort), Sign, Verify
  cmd/
    demo/
      main.go                signs a transfer request, verifies it, shows a tamper failing
  qsign_test.go              canonical == url.Values.Encode; escaping; sign round-trip; tamper
```

Files: `qsign.go`, `cmd/demo/main.go`, `qsign_test.go`.
Implement: `Canonical(params map[string]string) string`, `Sign(secret []byte, method, path string, params map[string]string) []byte` (HMAC-SHA256), and `Verify` using `hmac.Equal`.
Test: `Canonical` is byte-stable and equals `url.Values.Encode`; spaces/unicode/reserved chars encode identically; a round-trip signs and verifies, a tampered param does not.
Verify: `go test -count=1 -race ./...`

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/09-strings-builder-performance/09-canonical-querystring-signing-builder/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/09-strings-builder-performance/09-canonical-querystring-signing-builder
```

### Why canonicalization is a security property

An HMAC proves that whoever computed the signature knew the secret and signed *these
exact bytes*. If the client and server disagree by a single byte about what the
string-to-sign is, every signature mismatches and every request is rejected — or
worse, a sloppy canonicalization lets two different requests produce the same
string-to-sign, which is a signature-forgery vector. So the canonical form must be
deterministic and unambiguous: keys sorted (map iteration order is randomized in Go,
so unsorted output would differ between two runs of the *same* client), each key and
value percent-encoded so that a `&` or `=` inside a value cannot be confused with a
separator, and joined with a fixed scheme.

The encoding details are exactly where hand-rolling goes wrong. `url.QueryEscape`
encodes a space as `+` (not `%20`) in the query-component context, and escapes the
reserved characters `&`, `=`, `?`, `/`, `#` and non-ASCII bytes (so `café` becomes
`caf%C3%A9`). Get any of these wrong — encode a space as `%20`, or forget to escape an
embedded `&` — and your string-to-sign differs from the peer's and signing breaks, or
an attacker slips an unescaped `&extra=value` into a parameter and changes the meaning
of the signed request. `url.Values.Encode` already does sorting-plus-`QueryEscape`
correctly; the manual `Canonical` here must match it byte-for-byte, which the test
enforces. Building it by hand is instructive, but in production you would usually just
call `Values.Encode`.

With the canonical string in hand, `Sign` assembles the full string-to-sign
(`method\npath\ncanonicalQuery`) and runs it through `hmac.New(sha256.New, secret)`.
`Verify` recomputes and compares with `hmac.Equal`, which is constant-time — a plain
`bytes.Equal` on a MAC leaks timing information about how many leading bytes matched,
a real side channel, so MAC comparison always uses `hmac.Equal`.

Create `qsign.go`:

```go
package qsign

import (
	"crypto/hmac"
	"crypto/sha256"
	"net/url"
	"slices"
	"strings"
)

// Canonical builds a deterministic, percent-encoded, key-sorted query string.
// It matches url.Values.Encode byte-for-byte for single-valued parameters.
func Canonical(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(k))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(params[k]))
	}
	return b.String()
}

// Sign returns the HMAC-SHA256 of the canonical string-to-sign for the request.
func Sign(secret []byte, method, path string, params map[string]string) []byte {
	var b strings.Builder
	b.WriteString(method)
	b.WriteByte('\n')
	b.WriteString(path)
	b.WriteByte('\n')
	b.WriteString(Canonical(params))

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(b.String()))
	return mac.Sum(nil)
}

// Verify recomputes the signature and compares it in constant time.
func Verify(secret []byte, method, path string, params map[string]string, sig []byte) bool {
	return hmac.Equal(Sign(secret, method, path, params), sig)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/qsign"
)

func main() {
	secret := []byte("topsecret")
	params := map[string]string{"account": "42", "amount": "100.00", "note": "coffee & cake"}

	fmt.Println(qsign.Canonical(params))

	sig := qsign.Sign(secret, "POST", "/v1/transfer", params)
	fmt.Println("verified:", qsign.Verify(secret, "POST", "/v1/transfer", params, sig))

	tampered := map[string]string{"account": "42", "amount": "9999.00", "note": "coffee & cake"}
	fmt.Println("tampered verified:", qsign.Verify(secret, "POST", "/v1/transfer", tampered, sig))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
account=42&amount=100.00&note=coffee+%26+cake
verified: true
tampered verified: false
```

### Tests

The equivalence test builds the same parameters as a `url.Values` and asserts
`Canonical` equals its `Encode` — including spaces, unicode, and reserved characters.
The round-trip test signs with the parameters in one map and verifies with an
equal-but-differently-constructed map (proving order independence), then confirms a
tampered value fails.

Create `qsign_test.go`:

```go
package qsign

import (
	"fmt"
	"net/url"
	"testing"
)

func TestCanonicalMatchesValuesEncode(t *testing.T) {
	t.Parallel()

	params := map[string]string{
		"b":     "2",
		"a":     "1",
		"c d":   "e f",
		"q":     "café",
		"redir": "https://x/y?z=1&w=2",
	}
	v := url.Values{}
	for k, val := range params {
		v.Set(k, val)
	}
	if got, want := Canonical(params), v.Encode(); got != want {
		t.Fatalf("Canonical = %q\nValues.Encode = %q", got, want)
	}
}

func TestCanonicalSortedAndStable(t *testing.T) {
	t.Parallel()

	const want = "a=1&b=2&c=3"
	if got := Canonical(map[string]string{"c": "3", "a": "1", "b": "2"}); got != want {
		t.Fatalf("Canonical = %q, want %q", got, want)
	}
}

func TestSignRoundTrip(t *testing.T) {
	t.Parallel()

	secret := []byte("k")
	p1 := map[string]string{"a": "1", "b": "2"}
	p2 := map[string]string{"b": "2", "a": "1"} // same content, different literal order
	sig := Sign(secret, "GET", "/x", p1)

	if !Verify(secret, "GET", "/x", p2, sig) {
		t.Fatal("equivalent requests must produce the same signature")
	}
	tampered := map[string]string{"a": "1", "b": "3"}
	if Verify(secret, "GET", "/x", tampered, sig) {
		t.Fatal("tampered request must not verify")
	}
}

func ExampleCanonical() {
	fmt.Println(Canonical(map[string]string{"b": "2", "a": "1"}))
	// Output: a=1&b=2
}
```

## Review

`Canonical` is correct when it is byte-identical to `url.Values.Encode`: sorted keys,
each key and value run through `url.QueryEscape` (space as `+`, reserved and non-ASCII
bytes percent-encoded). That identity is the security property — a space encoded as
`%20` or an unescaped `&` would make your string-to-sign diverge from the peer's, and
the divergence either breaks every signature or opens a forgery seam. `Sign` builds the
`method\npath\nquery` string in one Builder and HMACs it; `Verify` compares with
`hmac.Equal` for constant-time safety, never `bytes.Equal`. The round-trip test proves
order-independence and that a single tampered byte fails verification.

## Resources

- [url.Values.Encode](https://pkg.go.dev/net/url#Values.Encode) — sorted, percent-encoded query encoding.
- [url.QueryEscape](https://pkg.go.dev/net/url#QueryEscape) — the query-component escaping rules.
- [crypto/hmac](https://pkg.go.dev/crypto/hmac) — `New` and constant-time `Equal`.

---

Prev: [08-sse-frame-writer-streaming.md](08-sse-frame-writer-streaming.md) | Back to [00-concepts.md](00-concepts.md) | Next: [10-builder-copylocks-and-reset-footguns.md](10-builder-copylocks-and-reset-footguns.md)
