# Exercise 3: Copy-vs-Alias Bug in a Parsed Header Slice

A middleware parses a comma-separated `X-Forwarded-For` header into a `[]string`
and stashes it on the request context for downstream handlers. To avoid an
allocation per request, an engineer reuses one scratch slice across requests — and
now request B's parse overwrites the very backing array that request A's stored
value points into. The stored identity of one request silently becomes another's.
This exercise reproduces the aliasing, then fixes it with `slices.Clone` before
storing.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
headerparse/               independent module: example.com/headerparse
  go.mod                   go 1.24
  headerparse.go           ParseInto(scratch, header); Middleware; context get/set
  cmd/
    demo/
      main.go              runnable demo: two requests through httptest
  headerparse_test.go      aliasing-bug test, clone-fix test, backing-array probe
```

- Files: `headerparse.go`, `cmd/demo/main.go`, `headerparse_test.go`.
- Implement: `ParseInto(scratch, header)` that splits and trims into a reused
  buffer (returns a view); `Middleware` that parses and stores a **cloned** slice
  on the context; context accessors.
- Test: two requests through one shared scratch buffer — without cloning the first
  stored slice is corrupted by the second parse; with `slices.Clone` both retain
  their own values.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

## Why "store for later" always needs an owned copy

`ParseInto` reuses its `scratch` argument: it truncates to `scratch[:0]` and appends
each trimmed field, so across many requests it allocates nothing after the buffer
has grown once. The catch is that the slice it returns is a *view* into that shared
`scratch` array. That view is fine to use *now*, inside the request, while the
buffer holds this request's data. It is poison the moment you *retain* it: the next
request reslices the same array to `scratch[:0]` and appends its own fields on top,
so any earlier retained view now reads the new request's data.

This is the central rule restated: a sub-slice is a view, and "store for later"
always needs an owned copy. The middleware stores the parsed IPs on the request
context so a downstream handler can read them — that is a value outliving the
parser's frame, so it must be detached. `slices.Clone` copies the fields into a
fresh, right-sized array that belongs to this request alone. Request B's parse can
churn the scratch buffer all it likes; request A's cloned slice is untouched.

Note we still parse into a reused scratch buffer (cheap) and clone only the final
result (one allocation, of exactly the size needed) — the best of both: no
per-token allocation on the hot path, and a safe owned value at the boundary where
it escapes.

Create `headerparse.go`:

```go
package headerparse

import (
	"context"
	"net/http"
	"slices"
	"strings"
)

type ctxKey int

const forwardedKey ctxKey = 0

// ParseInto splits a comma-separated header into trimmed fields, reusing scratch
// as the destination. The returned slice is a VIEW into scratch and is valid only
// until scratch is next reused. Clone it before storing it anywhere.
func ParseInto(scratch []string, header string) []string {
	scratch = scratch[:0]
	if header == "" {
		return scratch
	}
	for _, part := range strings.Split(header, ",") {
		if p := strings.TrimSpace(part); p != "" {
			scratch = append(scratch, p)
		}
	}
	return scratch
}

// WithForwardedFor returns a context carrying an owned copy of ips.
func WithForwardedFor(ctx context.Context, ips []string) context.Context {
	return context.WithValue(ctx, forwardedKey, ips)
}

// ForwardedForFrom returns the stored forwarded-for chain, or nil.
func ForwardedForFrom(ctx context.Context) []string {
	ips, _ := ctx.Value(forwardedKey).([]string)
	return ips
}

// Middleware parses X-Forwarded-For into an owned slice and stores it on the
// request context. It clones the parse result so a reused scratch buffer in a
// later request cannot corrupt this request's stored value.
func Middleware(scratch []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		view := ParseInto(scratch, r.Header.Get("X-Forwarded-For"))
		owned := slices.Clone(view)
		next.ServeHTTP(w, r.WithContext(WithForwardedFor(r.Context(), owned)))
	})
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/headerparse"
)

func main() {
	// One scratch buffer shared across all requests (the reuse that causes the bug
	// unless the middleware clones).
	scratch := make([]string, 0, 8)

	var captured []string
	handler := headerparse.Middleware(scratch, http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			captured = headerparse.ForwardedForFrom(r.Context())
		}))

	reqA := httptest.NewRequest(http.MethodGet, "/a", nil)
	reqA.Header.Set("X-Forwarded-For", "10.0.0.1, 10.0.0.2")
	handler.ServeHTTP(httptest.NewRecorder(), reqA)
	storedA := captured

	reqB := httptest.NewRequest(http.MethodGet, "/b", nil)
	reqB.Header.Set("X-Forwarded-For", "192.168.1.9")
	handler.ServeHTTP(httptest.NewRecorder(), reqB)
	storedB := captured

	fmt.Println("request A chain:", storedA)
	fmt.Println("request B chain:", storedB)
	fmt.Println("A survived B:", storedA[0] == "10.0.0.1")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
request A chain: [10.0.0.1 10.0.0.2]
request B chain: [192.168.1.9]
A survived B: true
```

## Tests

The first test drives the raw parser twice through one scratch buffer with *no*
clone and asserts the first retained view is corrupted by the second parse — the
bug, pinned. The second test does the same with `slices.Clone` and asserts both
stored slices keep their own values. The third probes that the cloned slices have
independent backing arrays by appending to one and checking the other is unaffected.

Create `headerparse_test.go`:

```go
package headerparse

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// TestReusedScratchAliases documents the bug: retaining the view returned by
// ParseInto across a second parse corrupts the first retained value.
func TestReusedScratchAliases(t *testing.T) {
	t.Parallel()
	scratch := make([]string, 0, 8)

	viewA := ParseInto(scratch, "10.0.0.1, 10.0.0.2") // retained WITHOUT clone
	if !slices.Equal(viewA, []string{"10.0.0.1", "10.0.0.2"}) {
		t.Fatalf("viewA parsed wrong: %v", viewA)
	}

	_ = ParseInto(scratch, "192.168.1.9") // reuses the same backing array

	if viewA[0] == "10.0.0.1" {
		t.Fatal("expected the reused scratch to corrupt viewA, but it survived")
	}
	if viewA[0] != "192.168.1.9" {
		t.Fatalf("viewA[0] = %q; expected it overwritten by second parse", viewA[0])
	}
}

// TestCloneIsolates proves the fix: cloning before retaining keeps each value.
func TestCloneIsolates(t *testing.T) {
	t.Parallel()
	scratch := make([]string, 0, 8)

	ownedA := slices.Clone(ParseInto(scratch, "10.0.0.1, 10.0.0.2"))
	ownedB := slices.Clone(ParseInto(scratch, "192.168.1.9"))

	if !slices.Equal(ownedA, []string{"10.0.0.1", "10.0.0.2"}) {
		t.Fatalf("ownedA = %v; want [10.0.0.1 10.0.0.2]", ownedA)
	}
	if !slices.Equal(ownedB, []string{"192.168.1.9"}) {
		t.Fatalf("ownedB = %v; want [192.168.1.9]", ownedB)
	}

	// Independent backing arrays: appending to one cannot touch the other.
	ownedA = append(ownedA, "extra")
	if ownedB[0] != "192.168.1.9" {
		t.Fatalf("append to ownedA leaked into ownedB: %v", ownedB)
	}
}

// TestMiddlewareStoresOwnedCopy drives two requests through one shared scratch and
// asserts the middleware's clone keeps each request's stored chain intact.
func TestMiddlewareStoresOwnedCopy(t *testing.T) {
	t.Parallel()
	scratch := make([]string, 0, 8)

	var stored [][]string
	h := Middleware(scratch, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stored = append(stored, ForwardedForFrom(r.Context()))
	}))

	for _, xff := range []string{"10.0.0.1, 10.0.0.2", "192.168.1.9"} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("X-Forwarded-For", xff)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}

	if !slices.Equal(stored[0], []string{"10.0.0.1", "10.0.0.2"}) {
		t.Fatalf("request A chain corrupted: %v", stored[0])
	}
	if !slices.Equal(stored[1], []string{"192.168.1.9"}) {
		t.Fatalf("request B chain wrong: %v", stored[1])
	}
}
```

## Review

The middleware is correct when each request's stored chain is exactly what that
request sent, regardless of how many other requests share the scratch buffer. The
aliasing test proves the danger with a bare view; the clone test and the middleware
test prove the fix. The mental error is treating `ParseInto`'s result as an owned
value because it "came back from a function" — it is a view into a buffer the next
call will reuse. The discipline: parse into a reused buffer for speed, but clone at
the exact boundary where a value escapes the request (onto a context, into a cache,
across a channel). Run `go test -race`; the parser holds no shared state, but the
race detector confirms the middleware does not either.

## Resources

- [`slices.Clone`](https://pkg.go.dev/slices#Clone)
- [`context.WithValue`](https://pkg.go.dev/context#WithValue)
- [`strings.Split`](https://pkg.go.dev/strings#Split)
- [Go Wiki: SliceTricks](https://go.dev/wiki/SliceTricks)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-three-index-batch-handoff.md](02-three-index-batch-handoff.md) | Next: [04-filter-connections-in-place.md](04-filter-connections-in-place.md)
